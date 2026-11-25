package judging

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"podtest/internal/artifact"
	"podtest/internal/db"
	pb "podtest/internal/invokerpb"
)

func TestArtifactControlIntegration(t *testing.T) {
	if os.Getenv("PODTEST_ARTIFACT_ACCEPTANCE") != "1" {
		t.Skip("requires disposable PostgreSQL and S3")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store, err := artifact.New(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	src, err := store.Put(ctx, "", "source", "text/plain", []byte("source"))
	if err != nil {
		t.Fatal(err)
	}
	bundle, _, err := artifact.BuildBundle("scripted_lua", 1, 1, []artifact.BundleFile{{Path: "runner.lua", Data: []byte("return 0\n")}})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put(ctx, "", "revision-bundle", "application/x-tar", bundle)
	if err != nil {
		t.Fatal(err)
	}
	lease := &pb.RunLease{InvokerId: uuid.NewString(), ProcessEpoch: uuid.NewString()}
	var user, contest, revision, language string
	err = pool.QueryRow(ctx, `WITH u AS (INSERT INTO users(display_name) VALUES('fixture') RETURNING id),
 c AS (INSERT INTO contests(title,created_by) SELECT 'fixture',id FROM u RETURNING id),
 p AS (INSERT INTO problems(title,created_by) SELECT 'fixture',id FROM u RETURNING id),
 r AS (INSERT INTO problem_revisions(problem_id,revision_number,execution_format,manifest,bundle_artifact_id,digest) SELECT id,1,'scripted_lua','{"package_format":"ustar_v1","execution_ready":false}',$1,$2 FROM p RETURNING id),
 lang AS (INSERT INTO languages(code,title) VALUES($3,'fixture') RETURNING id),
 rl AS (INSERT INTO problem_revision_languages(revision_id,language_id) SELECT r.id,lang.id FROM r,lang),
 st AS (INSERT INTO problem_statements(revision_id,locale,source,rendered_html,renderer_version,digest) SELECT id,'en','fixture','fixture','1',$2 FROM r)
 SELECT u.id,c.id,r.id,lang.id FROM u,c,r,lang`, ref.ID, ref.Digest, uuid.NewString()).Scan(&user, &contest, &revision, &language)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE problem_revisions SET status='published',published_at=now() WHERE id=$1`, revision); err != nil {
		t.Fatal(err)
	}
	err = pool.QueryRow(ctx, `WITH
 cp AS (INSERT INTO contest_problems(contest_id,problem_id,problem_revision_id,position,max_score) SELECT $1,problem_id,id,1,1 FROM problem_revisions WHERE id=$2 RETURNING id,contest_id,problem_id,problem_revision_id),
 part AS (INSERT INTO contest_participants(contest_id,user_id) VALUES($1,$3) RETURNING id,contest_id,user_id),
 s AS (INSERT INTO submissions(contest_problem_id,participant_id,contest_id,problem_id,submitted_by,problem_revision_id,language_id,source_artifact_id,digest) SELECT cp.id,part.id,cp.contest_id,cp.problem_id,part.user_id,cp.problem_revision_id,$4,$5,$6 FROM cp,part RETURNING id,problem_revision_id),
 i AS (INSERT INTO invokers(id,protocol_version,capabilities,capacity,load) VALUES($7,1,ARRAY['scripted_lua'],1,0) RETURNING id)
 INSERT INTO judging_attempts(submission_id,attempt_number,target_problem_revision_id,status,trigger_reason,invoker_id,lease_until,max_score)
 SELECT s.id,1,s.problem_revision_id,'assigned','fixture',i.id,now()+interval '2 minutes',1 FROM s,i RETURNING id,run_id`,
		contest, revision, user, language, src.ID, src.Digest, lease.InvokerId).Scan(&lease.AttemptId, &lease.RunId)
	if err != nil {
		t.Fatal(err)
	}
	claim := &pb.ClaimRequest{AttemptId: lease.AttemptId, RunId: lease.RunId, InvokerId: lease.InvokerId, ProcessEpoch: lease.ProcessEpoch}
	checkUnreadyDispatch(t, ctx, pool, lease.AttemptId)
	handler := &recoveryControl{
		ArtifactControl: &ArtifactControl{Pool: pool, Store: store},
		beforeCommit:    make(chan struct{}, 1),
		committed:       make(chan struct{}, 1),
		reported:        make(chan *pb.Event, 2),
	}
	control, anonymous, tlsEnv := artifactClients(t, lease.InvokerId, handler)
	if _, err = anonymous.Heartbeat(ctx, &pb.HeartbeatRequest{InvokerId: lease.InvokerId, ProtocolVersion: 1, Capacity: 1, ExecutionFormats: []string{"scripted_lua"}}); status.Code(err) != codes.Unavailable {
		t.Fatalf("unauthenticated Heartbeat: %v", err)
	}
	if _, err = control.Heartbeat(ctx, &pb.HeartbeatRequest{InvokerId: lease.InvokerId, ProtocolVersion: 1, Capacity: 2, Load: 1, ExecutionFormats: []string{"scripted_lua"}}); err != nil {
		t.Fatalf("authenticated Heartbeat: %v", err)
	}
	var capacity, load int
	if err = pool.QueryRow(ctx, `SELECT capacity,load FROM invokers WHERE id=$1`, lease.InvokerId).Scan(&capacity, &load); err != nil || capacity != 2 || load != 1 {
		t.Fatalf("heartbeat state was not persisted: capacity=%d load=%d err=%v", capacity, load, err)
	}
	if _, err = control.Heartbeat(ctx, &pb.HeartbeatRequest{InvokerId: lease.InvokerId, ProtocolVersion: 1}); err != nil {
		t.Fatalf("zero-capacity liveness Heartbeat: %v", err)
	}
	if err = pool.QueryRow(ctx, `SELECT capacity,load FROM invokers WHERE id=$1`, lease.InvokerId).Scan(&capacity, &load); err != nil || capacity != 0 || load != 0 {
		t.Fatalf("zero-capacity heartbeat was not persisted: capacity=%d load=%d err=%v", capacity, load, err)
	}
	unknownID := uuid.NewString()
	unknownInvoker, _, _ := artifactClients(t, unknownID, handler)
	if _, err = unknownInvoker.Heartbeat(ctx, &pb.HeartbeatRequest{InvokerId: unknownID, ProtocolVersion: 1}); err != nil {
		t.Fatalf("unknown Invoker liveness Heartbeat: %v", err)
	}
	var registered bool
	if err = pool.QueryRow(ctx, `SELECT EXISTS(SELECT FROM invokers WHERE id=$1)`, unknownID).Scan(&registered); err != nil || registered {
		t.Fatalf("liveness-only heartbeat registered an Invoker: registered=%t err=%v", registered, err)
	}
	if _, err = control.Heartbeat(ctx, &pb.HeartbeatRequest{InvokerId: lease.InvokerId, ProtocolVersion: 1, Capacity: 1, ExecutionFormats: []string{"unsupported"}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unsupported Heartbeat capability: %v", err)
	}
	if _, err = anonymous.Claim(ctx, claim); status.Code(err) != codes.Unavailable {
		t.Fatalf("unauthenticated Claim: %v", err)
	}
	wrongIdentity := proto.CloneOf(claim)
	wrongIdentity.InvokerId = uuid.NewString()
	if _, err = control.Claim(ctx, wrongIdentity); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("mismatched certificate identity: %v", err)
	}
	if binary := os.Getenv("PODTEST_INVOKER_BINARY"); binary != "" {
		checkInvokerArtifactRecovery(t, ctx, binary, tlsEnv, pool, store, lease, handler)
		if err := pool.QueryRow(ctx, `SELECT process_epoch::text FROM judging_attempts WHERE id=$1`, lease.AttemptId).Scan(&lease.ProcessEpoch); err != nil {
			t.Fatal(err)
		}
		claim.ProcessEpoch = lease.ProcessEpoch
	}
	inputs, err := control.Claim(ctx, claim)
	if err != nil || inputs.GetBundle().GetSizeBytes() != uint64(len(bundle)) || len(inputs.GetFencingToken()) != 32 {
		t.Fatalf("Claim: %v", err)
	}
	lease.FencingToken = inputs.FencingToken
	if _, err := pool.Exec(ctx, `UPDATE judging_attempts SET lease_until=clock_timestamp()+interval '30 seconds' WHERE id=$1`, lease.AttemptId); err != nil {
		t.Fatal(err)
	}
	wrongLease := proto.CloneOf(lease)
	wrongLease.FencingToken = make([]byte, 32)
	if _, err := control.RenewLease(ctx, wrongLease); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("renewal with wrong fence: %v", err)
	}
	renewed, err := control.RenewLease(ctx, lease)
	if err != nil || renewed.GetLeaseExpiresUnix() < time.Now().Add(90*time.Second).Unix() {
		t.Fatalf("expected extended lease: %v", err)
	}
	refresh, err := control.RefreshArtifacts(ctx, lease)
	if err != nil || !proto.Equal(inputs.Bundle, refresh.GetBundle()) || !proto.Equal(inputs.Source, refresh.GetSource()) {
		t.Fatalf("refresh changed descriptors: %v", err)
	}
	if zig := os.Getenv("PODTEST_ZIG_ARTIFACT_CLIENT"); zig != "" {
		workspace := t.TempDir()
		runZigInputs(t, ctx, zig, inputs.BundleUrl, workspace, inputs.Bundle, inputs.SourceUrl, inputs.Source)
		if b, err := os.ReadFile(filepath.Join(workspace, "workspace", "runner.lua")); err != nil || string(b) != "return 0\n" {
			t.Fatalf("Zig workspace runner: %v", err)
		}
		if b, err := os.ReadFile(filepath.Join(workspace, "workspace", "solution", "source")); err != nil || string(b) != "source" {
			t.Fatalf("Zig workspace source: %v", err)
		}
	}
	body := "integration artifact\n"
	sum := sha256.Sum256([]byte(body))
	create := &pb.CreateArtifactUploadRequest{Lease: lease, ClientArtifactId: uuid.NewString(), Kind: "build_log", Artifact: &pb.ArtifactDescriptor{Sha256: sum[:], SizeBytes: uint64(len(body))}}
	target, err := control.CreateArtifactUpload(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	replay := proto.CloneOf(create)
	replay.Artifact.SizeBytes++
	if _, err := control.CreateArtifactUpload(ctx, replay); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("descriptor-changing replay: %v", err)
	}
	again, err := control.RenewArtifactUpload(ctx, &pb.RenewArtifactUploadRequest{Lease: lease, ArtifactId: target.ArtifactId})
	if err != nil || target.ArtifactId != again.GetArtifactId() {
		t.Fatalf("renew: %v", err)
	}
	target = again
	finish := &pb.CompleteArtifactUploadRequest{Lease: lease, ArtifactId: target.ArtifactId}
	if _, err = control.CompleteArtifactUpload(ctx, finish); status.Code(err) != codes.NotFound {
		t.Fatalf("complete before PUT: %v", err)
	}
	unknown := proto.CloneOf(finish)
	unknown.ArtifactId = uuid.NewString()
	if _, err = control.CompleteArtifactUpload(ctx, unknown); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("complete without grant: %v", err)
	}
	if zig := os.Getenv("PODTEST_ZIG_ARTIFACT_CLIENT"); zig != "" {
		runZigArtifact(t, ctx, zig, "put", target.PutUrl, t.TempDir(), create.Artifact)
	} else {
		r, err := http.NewRequestWithContext(ctx, http.MethodPut, target.PutUrl, strings.NewReader(body))
		if err != nil {
			t.Fatal("invalid PUT request")
		}
		r.Header.Set("X-Amz-Checksum-Sha256", target.RequiredHeaders["x-amz-checksum-sha256"])
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal("PUT failed")
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("PUT status %d", resp.StatusCode)
		}
	}
	ack, err := control.CompleteArtifactUpload(ctx, finish)
	if err != nil || ack.GetDuplicate() {
		t.Fatalf("first completion: %v", err)
	}
	ack, err = control.CompleteArtifactUpload(ctx, finish)
	if err != nil || !ack.GetDuplicate() {
		t.Fatalf("repeat completion: %v", err)
	}
	confirmed, err := control.CreateArtifactUpload(ctx, create)
	if err != nil || !confirmed.GetComplete() || confirmed.GetArtifactId() != target.ArtifactId || confirmed.GetPutUrl() != "" {
		t.Fatalf("replay after completion: %v", err)
	}
	got, err := store.Read(ctx, target.ArtifactId, 1024)
	if err != nil || string(got) != body {
		t.Fatalf("final artifact read: %v", err)
	}
	var submissionID string
	var last uint64
	if err := pool.QueryRow(ctx, `SELECT submission_id,last_sequence FROM judging_attempts WHERE id=$1`, lease.AttemptId).Scan(&submissionID, &last); err != nil {
		t.Fatal(err)
	}
	eventID := uuid.New()
	event := &pb.Event{EventId: eventID[:], AttemptId: lease.AttemptId, RunId: lease.RunId,
		InvokerId: lease.InvokerId, SubmissionId: submissionID, ArtifactId: target.ArtifactId,
		FencingToken: lease.FencingToken, Kind: pb.Event_ARTIFACT_CREATED, Sequence: last + 1,
		SchemaVersion: 1, OccurredAtUnixMillis: time.Now().UnixMilli()}
	invalid := proto.CloneOf(event)
	invalid.FencingToken = make([]byte, 32)
	if _, err := control.Report(ctx, invalid); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Report with wrong fence: %v", err)
	}
	invalid = proto.CloneOf(event)
	invalid.ArtifactId = uuid.NewString()
	if _, err := control.Report(ctx, invalid); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Report without completed grant: %v", err)
	}
	if ack, err := control.Report(ctx, event); err != nil || ack.GetDuplicate() {
		t.Fatalf("first Report: %v", err)
	}
	invalid = proto.CloneOf(event)
	invalid.OccurredAtUnixMillis++
	if _, err := control.Report(ctx, invalid); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("changed event replay: %v", err)
	}
	lease.FencingToken = make([]byte, 32)
	if _, err = control.CreateArtifactUpload(ctx, create); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("wrong fence: %v", err)
	}
	lease.FencingToken = inputs.FencingToken
	if _, err := pool.Exec(ctx, `UPDATE judging_attempts SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, lease.AttemptId); err != nil {
		t.Fatal(err)
	}
	if _, err := control.CompleteArtifactUpload(ctx, finish); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("completion on expired lease: %v", err)
	}
	if _, err := control.RenewLease(ctx, lease); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("renewal of expired run: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO queue_entries(attempt_id,contest_id,participant_id,status)
		SELECT j.id,s.contest_id,s.participant_id,'assigned' FROM judging_attempts j JOIN submissions s ON s.id=j.submission_id WHERE j.id=$1`, lease.AttemptId); err != nil {
		t.Fatal(err)
	}
	if err := recoverExpired(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var attemptState, queueState string
	if err := pool.QueryRow(ctx, `SELECT j.status,q.status FROM judging_attempts j JOIN queue_entries q ON q.attempt_id=j.id WHERE j.id=$1`, lease.AttemptId).Scan(&attemptState, &queueState); err != nil || attemptState != "recovery" || queueState != "done" {
		t.Fatalf("expired lease recovery: attempt=%s queue=%s err=%v", attemptState, queueState, err)
	}
	if ack, err := control.Report(ctx, event); err != nil || !ack.GetDuplicate() {
		t.Fatalf("exact Report replay on expired lease: %v", err)
	}
	newID := uuid.New()
	event.EventId, event.Sequence = newID[:], event.Sequence+1
	if _, err := control.Report(ctx, event); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("new Report on expired lease: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE judging_attempts SET status='cancelled',lease_until=clock_timestamp()+interval '30 seconds' WHERE id=$1`, lease.AttemptId); err != nil {
		t.Fatal(err)
	}
	if _, err := control.RenewLease(ctx, lease); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("renewal of cancelled run: %v", err)
	}
	checkTerminalReport(t, ctx, pool, control, lease, submissionID)
}

func checkTerminalReport(t *testing.T, ctx context.Context, pool *pgxpool.Pool, control pb.ControlClient, previous *pb.RunLease, submission string) {
	t.Helper()
	for i, outcome := range []string{"infrastructure_error", "timeout"} {
		claim := &pb.ClaimRequest{InvokerId: previous.InvokerId, ProcessEpoch: uuid.NewString()}
		if err := pool.QueryRow(ctx, `INSERT INTO judging_attempts(submission_id,attempt_number,target_problem_revision_id,status,trigger_reason,invoker_id,lease_until,max_score)
			SELECT submission_id,$2,target_problem_revision_id,'assigned','fixture',invoker_id,clock_timestamp()+interval '2 minutes',max_score
			FROM judging_attempts WHERE id=$1 RETURNING id,run_id`, previous.AttemptId, i+3).Scan(&claim.AttemptId, &claim.RunId); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO queue_entries(attempt_id,contest_id,participant_id,status)
			SELECT $1,contest_id,participant_id,'assigned' FROM submissions WHERE id=$2`, claim.AttemptId, submission); err != nil {
			t.Fatal(err)
		}
		granted, err := control.Claim(ctx, claim)
		if err != nil {
			t.Fatal(err)
		}
		id := uuid.New()
		event := &pb.Event{EventId: id[:], AttemptId: claim.AttemptId, RunId: claim.RunId, SubmissionId: submission,
			InvokerId: claim.InvokerId, FencingToken: granted.FencingToken, Sequence: 1, SchemaVersion: 1,
			OccurredAtUnixMillis: time.Now().UnixMilli(), Kind: pb.Event_RUN_FINISHED, Outcome: outcome}
		invalid := proto.CloneOf(event)
		invalid.Outcome = "success"
		if _, err := control.Report(ctx, invalid); status.Code(err) != codes.Unimplemented {
			t.Fatalf("unsupported success report: %v", err)
		}
		invalid = proto.CloneOf(event)
		invalid.FencingToken = make([]byte, 32)
		if _, err := control.Report(ctx, invalid); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("terminal report with wrong fence: %v", err)
		}
		if ack, err := control.Report(ctx, event); err != nil || ack.GetDuplicate() {
			t.Fatalf("first terminal Report: %v", err)
		}
		if ack, err := control.Report(ctx, event); err != nil || !ack.GetDuplicate() {
			t.Fatalf("terminal Report replay: %v", err)
		}
		invalid = proto.CloneOf(event)
		invalid.OccurredAtUnixMillis++
		if _, err := control.Report(ctx, invalid); status.Code(err) != codes.AlreadyExists {
			t.Fatalf("changed terminal Report replay: %v", err)
		}
		anotherID := uuid.New()
		invalid.EventId, invalid.Sequence = anotherID[:], 2
		if _, err := control.Report(ctx, invalid); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("event after terminal Report: %v", err)
		}
		wantState := []string{"error", "timeout"}[i]
		var state, queued, storedOutcome string
		var sequence int
		err = pool.QueryRow(ctx, `SELECT j.status,q.status,j.last_sequence,e.payload->>'outcome' FROM judging_attempts j
			JOIN queue_entries q ON q.attempt_id=j.id JOIN judging_events e ON e.attempt_id=j.id AND e.sequence=j.last_sequence
			WHERE j.id=$1 AND j.finished_at IS NOT NULL AND j.verdict IS NULL AND j.base_score IS NULL`, claim.AttemptId).
			Scan(&state, &queued, &sequence, &storedOutcome)
		if err != nil || state != wantState || queued != "done" || sequence != 1 || storedOutcome != outcome {
			t.Fatalf("terminal Report not committed: state=%s queue=%s sequence=%d outcome=%s err=%v", state, queued, sequence, storedOutcome, err)
		}
	}
}

type recordingPublisher struct {
	nats.JetStreamContext
	calls int
}

func (p *recordingPublisher) Publish(string, []byte, ...nats.PubOpt) (*nats.PubAck, error) {
	p.calls++
	return &nats.PubAck{}, nil
}

func checkUnreadyDispatch(t *testing.T, ctx context.Context, pool *pgxpool.Pool, assigned string) {
	t.Helper()
	var queued string
	err := db.Tx(ctx, pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO judging_attempts(submission_id,attempt_number,target_problem_revision_id,status,trigger_reason,max_score)
			SELECT submission_id,attempt_number+1,target_problem_revision_id,'queued','fixture',max_score FROM judging_attempts WHERE id=$1 RETURNING id`, assigned).Scan(&queued); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO queue_entries(attempt_id,contest_id,participant_id,status)
			SELECT j.id,s.contest_id,s.participant_id,'queued' FROM judging_attempts j JOIN submissions s ON s.id=j.submission_id WHERE j.id=$1`, queued); err != nil {
			return err
		}
		return enqueue(ctx, tx, queued)
	})
	if err != nil {
		t.Fatal(err)
	}
	publisher := &recordingPublisher{}
	if err := dispatch(ctx, pool, publisher); err != nil || publisher.calls != 0 {
		t.Fatalf("unready revision reached JetStream: calls=%d err=%v", publisher.calls, err)
	}
	var attemptStatus, queueStatus string
	var published bool
	err = pool.QueryRow(ctx, `SELECT j.status,q.status,e.published_at IS NOT NULL FROM judging_attempts j
		JOIN queue_entries q ON q.attempt_id=j.id JOIN outbox_events e ON e.dedup_key=j.id::text WHERE j.id=$1`, queued).
		Scan(&attemptStatus, &queueStatus, &published)
	if err != nil || attemptStatus != "queued" || queueStatus != "queued" || published {
		t.Fatalf("unready attempt did not remain unpublished and queued: attempt=%s queue=%s published=%t err=%v", attemptStatus, queueStatus, published, err)
	}
}

func runZigArtifact(t *testing.T, ctx context.Context, binary, mode, signedURL, workspace string, d *pb.ArtifactDescriptor) {
	t.Helper()
	spool := filepath.Join(t.TempDir(), "spool")
	if err := os.Mkdir(spool, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, binary, mode, os.Getenv("S3_URL"), os.Getenv("SSL_CERT_FILE"), spool, workspace, fmt.Sprint(d.SizeBytes), hex.EncodeToString(d.Sha256), "", "")
	cmd.Stdin = strings.NewReader(signedURL + "\n")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Zig %s: %v\n%s", mode, err, output)
	}
}

func runZigInputs(t *testing.T, ctx context.Context, binary, bundleURL, workspace string, bundle *pb.ArtifactDescriptor, sourceURL string, source *pb.ArtifactDescriptor) {
	t.Helper()
	spool := filepath.Join(t.TempDir(), "spool")
	if err := os.Mkdir(spool, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, binary, "bundle", os.Getenv("S3_URL"), os.Getenv("SSL_CERT_FILE"), spool, workspace,
		fmt.Sprint(bundle.SizeBytes), hex.EncodeToString(bundle.Sha256), fmt.Sprint(source.SizeBytes), hex.EncodeToString(source.Sha256))
	cmd.Stdin = strings.NewReader(bundleURL + "\n" + sourceURL)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Zig bundle: %v\n%s", err, output)
	}
}

func artifactClients(t *testing.T, invokerID string, control pb.ControlServer) (pb.ControlClient, pb.ControlClient, []string) {
	t.Helper()
	now := time.Now()
	key := func() *ecdsa.PrivateKey {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	caKey := key()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	cert := func(serial int64, name string, usage x509.ExtKeyUsage) tls.Certificate {
		k := key()
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
		if usage == x509.ExtKeyUsageServerAuth {
			leaf.IPAddresses = []net.IP{net.IPv4(127, 0, 0, 1)}
		}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &k.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert(2, "server", x509.ExtKeyUsageServerAuth)}, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert})))
	pb.RegisterControlServer(srv, control)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	client := func(certs ...tls.Certificate) pb.ControlClient {
		conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: certs})))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return pb.NewControlClient(conn)
	}
	identity := cert(3, invokerID, x509.ExtKeyUsageClientAuth)
	keyDER, err := x509.MarshalPKCS8PrivateKey(identity.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(name, typ string, der []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	return client(identity), client(), []string{
		"PODTEST_CONTROL_ENDPOINT=" + ln.Addr().String(),
		"PODTEST_CONTROL_CA=" + write("ca.pem", "CERTIFICATE", caDER),
		"PODTEST_CONTROL_CERT=" + write("client.pem", "CERTIFICATE", identity.Certificate[0]),
		"PODTEST_CONTROL_KEY=" + write("key.pem", "PRIVATE KEY", keyDER),
		"PODTEST_ARTIFACT_ORIGIN=" + os.Getenv("S3_URL"), "PODTEST_ARTIFACT_CA=" + os.Getenv("SSL_CERT_FILE"),
	}
}

type recoveryControl struct {
	*ArtifactControl
	requireRefresh atomic.Bool
	changeClaim    atomic.Bool
	refreshes      atomic.Int32
	pauseBefore    atomic.Int32
	dropNext       atomic.Bool
	beforeCommit   chan struct{}
	committed      chan struct{}
	dropReport     atomic.Bool
	reported       chan *pb.Event
	reportReplays  atomic.Int32
}

func (s *recoveryControl) Report(ctx context.Context, req *pb.Event) (*pb.Acknowledgement, error) {
	ack, err := s.ArtifactControl.Report(ctx, req)
	if err != nil {
		return nil, err
	}
	if ack.Duplicate {
		s.reportReplays.Add(1)
	}
	if s.dropReport.Swap(false) {
		s.reported <- proto.CloneOf(req)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return ack, nil
}

func (s *recoveryControl) Claim(ctx context.Context, req *pb.ClaimRequest) (*pb.ClaimResponse, error) {
	res, err := s.ArtifactControl.Claim(ctx, req)
	if err == nil && s.requireRefresh.Swap(false) {
		res.BundleUrl += "&fixture-invalidates-signature=1"
	}
	if err == nil && s.changeClaim.Swap(false) {
		res.Source.Sha256[0] ^= 1
	}
	return res, err
}

func (s *recoveryControl) RefreshArtifacts(ctx context.Context, req *pb.RunLease) (*pb.ClaimResponse, error) {
	s.refreshes.Add(1)
	return s.ArtifactControl.RefreshArtifacts(ctx, req)
}

func (s *recoveryControl) CompleteArtifactUpload(ctx context.Context, req *pb.CompleteArtifactUploadRequest) (*pb.Acknowledgement, error) {
	if remaining := s.pauseBefore.Load(); remaining > 0 && s.pauseBefore.CompareAndSwap(remaining, remaining-1) && remaining == 1 {
		s.beforeCommit <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ack, err := s.ArtifactControl.CompleteArtifactUpload(ctx, req)
	if err == nil && s.dropNext.Swap(false) {
		s.committed <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return ack, err
}

func checkInvokerArtifactRecovery(t *testing.T, ctx context.Context, binary string, env []string, pool *pgxpool.Pool, store *artifact.Store, lease *pb.RunLease, handler *recoveryControl) {
	t.Helper()
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name string, data []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	config, err := json.Marshal(map[string]any{"invoker_id": lease.InvokerId, "state_dir": state, "podman_socket": "/unused.sock"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := write("config.json", config)
	var submissionID string
	err = pool.QueryRow(ctx, `SELECT submission_id FROM judging_attempts WHERE id=$1`, lease.AttemptId).Scan(&submissionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Tx(ctx, pool, func(tx pgx.Tx) error { return enqueue(ctx, tx, lease.AttemptId) }); err != nil {
		t.Fatal(err)
	}
	var job []byte
	if err := pool.QueryRow(ctx, `SELECT payload FROM outbox_events WHERE topic=$1 AND dedup_key=$2`, Subject, lease.AttemptId).Scan(&job); err != nil {
		t.Fatal(err)
	}
	command := func(args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = append(os.Environ(), env...)
		return cmd
	}
	run := func(args ...string) {
		if output, err := command(args...).CombinedOutput(); err != nil {
			t.Fatalf("Invoker %s: %v\n%s", args[0], err, output)
		}
	}
	if port := os.Getenv("PODTEST_CLAIM_FIXTURE_PORT"); port != "" {
		checkClaimDelivery(t, port, cfg, state, job, lease.RunId, command)
	}
	if _, err := pool.Exec(ctx, `UPDATE judging_attempts SET lease_until=clock_timestamp()+interval '25 seconds' WHERE id=$1`, lease.AttemptId); err != nil {
		t.Fatal(err)
	}
	handler.requireRefresh.Store(true)
	run("prepare-assigned", cfg, write("job.json", job))
	var extended bool
	if err := pool.QueryRow(ctx, `SELECT lease_until>clock_timestamp()+interval '1 minute' FROM judging_attempts WHERE id=$1`, lease.AttemptId).Scan(&extended); err != nil || !extended {
		t.Fatalf("Invoker did not renew its short lease during preparation: extended=%t err=%v", extended, err)
	}
	if handler.refreshes.Load() != 1 {
		t.Fatal("Invoker did not refresh the rejected URL exactly once")
	}
	run("prepare-assigned", cfg, filepath.Join(dir, "job.json"))
	handler.changeClaim.Store(true)
	if output, err := command("prepare-assigned", cfg, filepath.Join(dir, "job.json")).CombinedOutput(); err == nil || !strings.Contains(string(output), "PinnedInputsChanged") {
		t.Fatalf("Invoker accepted changed pinned source after restart: %v\n%s", err, output)
	}
	if b, err := os.ReadFile(filepath.Join(state, "workspaces", lease.RunId, "solution", "source")); err != nil || string(b) != "source" {
		t.Fatalf("prepared source: %v", err)
	}
	clientID := uuid.NewString()
	body := []byte("retained across Invoker restart\n")
	payload := write("payload", body)
	run("stage-artifact", cfg, lease.RunId, clientID, "runner_log", payload)
	run("stage-artifact", cfg, lease.RunId, clientID, "runner_log", payload)
	changed := write("changed", []byte("different payload\n"))
	if output, err := command("stage-artifact", cfg, lease.RunId, clientID, "runner_log", changed).CombinedOutput(); err == nil || !strings.Contains(string(output), "UploadIntentMismatch") {
		t.Fatalf("Invoker accepted a changed staging payload: %v\n%s", err, output)
	}
	digest := sha256.Sum256([]byte(clientID))
	receipt := filepath.Join(state, "upload-journal", fmt.Sprintf("result-%x.json", digest))
	spooled := filepath.Join(state, "upload-spool", clientID)
	walPath := filepath.Join(state, "event-outbox.wal")
	interrupted := []string{
		filepath.Join(state, "input-spool", lease.RunId),
		filepath.Join(state, "workspaces", ".extract-0123456789abcdef0123456789abcdef"),
	}
	for _, path := range interrupted {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "partial"), []byte("interrupted"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	interrupt := func(point <-chan struct{}, stage string) {
		t.Helper()
		cmd := command("serve-artifacts", cfg)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
		select {
		case <-point:
		case <-time.After(5 * time.Second):
			t.Fatalf("Invoker did not reach %s", stage)
		}
		if _, err := os.Stat(receipt); !os.IsNotExist(err) {
			t.Fatalf("receipt before completion response: %v", err)
		}
		if _, err := os.Stat(spooled); err != nil {
			t.Fatal(err)
		}
	}
	// Completion first checks for an existing PUT and then runs again after PUT.
	handler.pauseBefore.Store(2)
	interrupt(handler.beforeCommit, "PUT before completion commit")
	for _, path := range interrupted {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("interrupted input staging still present: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(state, "workspaces", lease.RunId, "solution", "source")); err != nil {
		t.Fatalf("published workspace disappeared: %v", err)
	}
	handler.dropNext.Store(true)
	interrupt(handler.committed, "completion commit before response")
	handler.dropReport.Store(true)
	if _, err := pool.Exec(ctx, `UPDATE invokers SET capacity=1,load=1 WHERE id=$1`, lease.InvokerId); err != nil {
		t.Fatal(err)
	}
	var firstEvent *pb.Event
	func() {
		cmd := command("serve-artifacts", cfg)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
		select {
		case firstEvent = <-handler.reported:
		case <-time.After(5 * time.Second):
			t.Fatal("Invoker did not reach Report commit before ACK")
		}
		var capacity, load int
		if err := pool.QueryRow(ctx, `SELECT capacity,load FROM invokers WHERE id=$1`, lease.InvokerId).Scan(&capacity, &load); err != nil || capacity != 0 || load != 0 {
			t.Fatalf("Invoker did not clear scheduling capacity: capacity=%d load=%d err=%v", capacity, load, err)
		}
		if !proto.Equal(firstEvent, readArtifactEvent(t, walPath)) {
			t.Fatal("reported event did not match pending WAL event")
		}
	}()
	for i := 0; i < 2; i++ {
		cmd := command("serve-artifacts", cfg)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			scanner := bufio.NewScanner(stdout)
			for scanner.Scan() && !strings.Contains(scanner.Text(), `"operation":"artifact_recovery_scanned"`) {
			}
			if !strings.Contains(scanner.Text(), `"pending":0`) {
				t.Fatalf("artifact recovery scan failed: %v %s", scanner.Err(), scanner.Text())
			}
			id, err := os.ReadFile(receipt)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(spooled); !os.IsNotExist(err) {
				t.Fatalf("spool was not released: %v", err)
			}
			event := firstEvent
			if event.Kind != pb.Event_ARTIFACT_CREATED || event.ArtifactId != string(id) || event.SubmissionId != submissionID || event.RunId != lease.RunId || event.AttemptId != lease.AttemptId || event.InvokerId != lease.InvokerId || len(event.FencingToken) != 32 || len(event.EventId) != 16 || event.Sequence != 1 {
				t.Fatal("unexpected artifact event")
			}
			if got := handler.reportReplays.Load(); got != 1 {
				t.Fatalf("expected exactly one Report replay, got %d", got)
			}
			b, err := store.Read(ctx, string(id), 1024)
			if err != nil || string(b) != string(body) {
				t.Fatalf("recovered payload: %v", err)
			}
		}()
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM judging_events WHERE attempt_id=$1`, lease.AttemptId).Scan(&count); err != nil || count != 1 {
		t.Fatalf("stored event count %d: %v", count, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM invoker_upload_grants WHERE client_artifact_id=$1 AND completed_at IS NOT NULL`, clientID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("confirmed grant count %d: %v", count, err)
	}
}

func checkClaimDelivery(t *testing.T, port, cfg, state string, job []byte, runID string, command func(...string) *exec.Cmd) {
	t.Helper()
	nc, err := nats.Connect("nats://127.0.0.1:"+port, nats.Timeout(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := nc.JetStream(nats.MaxWait(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	stream, durable := "CLAIM_"+strings.ReplaceAll(runID, "-", ""), "invokers"
	subject := "test.claim." + runID
	if _, err = js.AddStream(&nats.StreamConfig{Name: stream, Subjects: []string{subject}, Storage: nats.MemoryStorage}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = js.DeleteStream(stream) }()
	if _, err = js.AddConsumer(stream, &nats.ConsumerConfig{Durable: durable, FilterSubject: subject, AckPolicy: nats.AckExplicitPolicy, AckWait: time.Second}); err != nil {
		t.Fatal(err)
	}
	if _, err = js.Publish(subject, job); err != nil {
		t.Fatal(err)
	}
	intents := filepath.Join(state, "claim-intents")
	if err = os.Mkdir(intents, 0700); err != nil {
		t.Fatal(err)
	}
	expiryPath := filepath.Join(intents, "expires-"+runID)
	if err = os.Mkdir(expiryPath, 0700); err != nil {
		t.Fatal(err)
	}
	invoke := func() ([]byte, error) {
		return command("test-claim-delivery", cfg, port, stream, durable, subject).CombinedOutput()
	}
	if out, err := invoke(); err == nil || !strings.Contains(string(out), "IsDir") {
		t.Fatalf("expected local lease persistence failure: %v %s", err, out)
	}
	info, err := js.ConsumerInfo(stream, durable)
	if err != nil || info.AckFloor.Stream != 0 || info.NumAckPending != 1 {
		t.Fatalf("delivery ACKed despite failed persistence: %v", err)
	}
	pinPath := filepath.Join(intents, "pin-"+runID)
	pin, err := os.ReadFile(pinPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(expiryPath); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second) // Wait for the fixture consumer's ACK deadline.
	if out, err := invoke(); err != nil {
		t.Fatalf("claim redelivery: %v %s", err, out)
	}
	info, err = js.ConsumerInfo(stream, durable)
	if err != nil || info.AckFloor.Stream != 1 || info.NumAckPending != 0 || info.Delivered.Consumer != 2 {
		t.Fatalf("expected confirmed ACK on redelivery: %v", err)
	}
	if replay, err := os.ReadFile(pinPath); err != nil || string(replay) != string(pin) {
		t.Fatalf("redelivery changed the pinned lease: %v", err)
	}
	if expiry, err := os.ReadFile(expiryPath); err != nil || len(expiry) == 0 {
		t.Fatalf("missing lease expiration after ACK: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state, "workspaces", runID)); !os.IsNotExist(err) {
		t.Fatalf("claim fixture unexpectedly prepared a workspace: %v", err)
	}
}

func readArtifactEvent(t *testing.T, path string) *pb.Event {
	t.Helper()
	wire, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const header = 28
	if len(wire) <= header+9 || string(wire[:4]) != "PTOB" || wire[4] != 2 || wire[5] != 5 || int(binary.LittleEndian.Uint32(wire[16:20])) != len(wire)-header {
		t.Fatal("expected exactly one run event in the WAL")
	}
	start := header + 9 + int(wire[header])
	if start >= len(wire) {
		t.Fatal("missing WAL event payload")
	}
	event := new(pb.Event)
	if err := proto.Unmarshal(wire[start:], event); err != nil {
		t.Fatal(err)
	}
	return event
}
