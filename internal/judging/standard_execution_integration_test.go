package judging

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"podtest/internal/artifact"
	"podtest/internal/db"
	pb "podtest/internal/invokerpb"
	"podtest/internal/problem"
)

func TestStandardSubmissionExecution(t *testing.T) {
	binary, url, socket, image := os.Getenv("PODTEST_STANDARD_WORKER"), os.Getenv("PODTEST_NATS_URL"), os.Getenv("PODTEST_PODMAN_SOCKET"), os.Getenv("PODTEST_STANDARD_SH_IMAGE_ID")
	if os.Getenv("PODTEST_ARTIFACT_ACCEPTANCE") != "1" || binary == "" {
		t.Skip("requires disposable PostgreSQL, HTTPS MinIO, JetStream and the Standard worker")
	}
	if url == "" || socket == "" || image == "" {
		t.Fatal("set PODTEST_NATS_URL, PODTEST_PODMAN_SOCKET and PODTEST_STANDARD_SH_IMAGE_ID")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(db.Migrate(ctx, pool))
	store, err := artifact.New(pool)
	must(err)
	must(store.Init(ctx))
	var user, contest, task string
	must(pool.QueryRow(ctx, `WITH u AS (INSERT INTO users(display_name) VALUES('Standard execution') RETURNING id),
		c AS (INSERT INTO contests(title,created_by,status) SELECT 'Standard execution',id,'open' FROM u RETURNING id),
		p AS (INSERT INTO problems(title,created_by) SELECT 'Echo',id FROM u RETURNING id),
		part AS (INSERT INTO contest_participants(contest_id,user_id) SELECT c.id,u.id FROM c,u),
		settings AS (INSERT INTO contest_settings(contest_id) SELECT id FROM c)
		SELECT u.id,c.id,p.id FROM u,c,p`).Scan(&user, &contest, &task))
	manager := problem.Manager{Pool: pool, Artifacts: store}
	revision, err := manager.Create(ctx, task, problem.RevisionInput{Format: "standard"})
	must(err)
	_, err = pool.Exec(ctx, `INSERT INTO problem_statements(revision_id,locale,source,rendered_html,renderer_version,digest) VALUES($1,'en','Echo','Echo','1',$2)`, revision.ID, fmt.Sprintf("%064x", 1))
	must(err)
	must(manager.SetLanguage(ctx, task, revision.ID, "posix_sh"))
	var archive bytes.Buffer
	zipper := zip.NewWriter(&archive)
	for name, data := range map[string]string{
		"standard.json": `{"version":1,"language":"posix_sh","build":"package_source","checker":"exact_bytes","limits":{"wall_ms":2000,"output_bytes":4096},"groups":[{"id":"main","scoring":"sum"}],"tests":[{"id":"echo","group":"main","input":"tests/input","answer":"tests/answer","points_milli":1000}]}`,
		"tests/input":   "hello\n", "tests/answer": "hello\n",
	} {
		file, err := zipper.Create(name)
		must(err)
		_, err = file.Write([]byte(data))
		must(err)
	}
	must(zipper.Close())
	must(manager.SetPackage(ctx, user, task, revision.ID, archive.Bytes()))
	must(manager.Validate(ctx, task, revision.ID))
	must(manager.Publish(ctx, user, task, revision.ID))
	cp, err := manager.Attach(ctx, user, contest, problem.AttachInput{ProblemID: task, RevisionID: revision.ID, MaxScore: "1", VerdictVisibility: "tests"})
	must(err)
	sub, err := (Submissions{Pool: pool, Artifacts: store}).Submit(ctx, user, contest, cp, "posix_sh", []byte("exec cat\n"))
	must(err)
	var attempt, run string
	must(pool.QueryRow(ctx, `SELECT id,run_id FROM judging_attempts WHERE submission_id=$1`, sub.ID).Scan(&attempt, &run))
	nc, err := nats.Connect(url)
	must(err)
	defer nc.Close()
	js, err := nc.JetStream()
	must(err)
	must(provisionStandardQueue(ctx, js))
	must(dispatch(ctx, pool, js))
	invoker := uuid.NewString()
	server := &standardCrashControl{ArtifactControl: &ArtifactControl{Pool: pool, Store: store}, committed: make(chan *pb.Event, 1)}
	control, _, tlsEnv := artifactClients(t, invoker, server)
	state := t.TempDir()
	must(os.Chmod(state, 0700))
	config := filepath.Join(t.TempDir(), "config.json")
	data, err := json.Marshal(map[string]any{"invoker_id": invoker, "state_dir": state, "podman_socket": socket, "libpod_api_version": os.Getenv("PODTEST_LIBPOD_API_VERSION")})
	must(err)
	must(os.WriteFile(config, data, 0600))
	command := func(mode string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, binary, mode, config)
		cmd.Env = append(os.Environ(), tlsEnv...)
		if os.Getenv("PODTEST_STANDARD_JS_IMAGE_ID") == "" {
			cmd.Env = append(cmd.Env, "PODTEST_STANDARD_JS_IMAGE_ID="+image)
		}
		return cmd
	}
	first := command("work-standard-once")
	var workerLog bytes.Buffer
	first.Stdout, first.Stderr = &workerLog, &workerLog
	must(first.Start())
	done := make(chan error, 1)
	go func() { done <- first.Wait() }()
	var terminal *pb.Event
	select {
	case terminal = <-server.committed:
		must(first.Process.Kill())
		if err := <-done; err == nil {
			t.Fatal("worker exited before the simulated crash")
		}
	case err := <-done:
		t.Fatalf("worker exited before terminal commit: %v\n%s", err, &workerLog)
	case <-ctx.Done():
		<-done
		t.Fatalf("terminal commit not observed: %v\n%s", ctx.Err(), &workerLog)
	}
	if out, err := command("work-standard-once").CombinedOutput(); err != nil {
		t.Fatalf("worker could not replay the terminal event: %v\n%s", err, out)
	}
	if server.replays.Load() != 1 {
		t.Fatalf("terminal duplicate ACKs = %d", server.replays.Load())
	}
	var result, verdict, score, testVerdict, groupScore, queue string
	must(pool.QueryRow(ctx, `SELECT j.status,j.verdict,j.base_score::text,t.verdict,g.score::text,q.status
		FROM judging_attempts j JOIN test_results t ON t.attempt_id=j.id AND t.test_id='echo'
		JOIN subgroup_results g ON g.attempt_id=j.id AND g.subgroup_id='main' JOIN queue_entries q ON q.attempt_id=j.id
		WHERE j.id=$1`, attempt).Scan(&result, &verdict, &score, &testVerdict, &groupScore, &queue))
	if result != "finished" || verdict != "accepted" || score != "1.000000" || testVerdict != "accepted" || groupScore != score || queue != "done" {
		t.Fatalf("persisted results: %q %q %q %q %q %q", result, verdict, score, testVerdict, groupScore, queue)
	}
	stale := proto.CloneOf(terminal)
	id := uuid.New()
	stale.EventId = id[:]
	stale.Sequence++
	stale.FencingToken = make([]byte, 32)
	if _, err := control.Report(ctx, stale); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Report with stale fence: %v", err)
	}
	info, err := js.ConsumerInfo(jobStream, jobConsumer)
	must(err)
	if info.NumAckPending != 0 || info.NumPending != 0 {
		t.Fatalf("queue still has unconfirmed deliveries: %+v", info)
	}
	if _, err := os.Stat(filepath.Join(state, "standard-runs", run, "done")); err != nil {
		t.Fatalf("local durable completion: %v", err)
	}

	// Lose a second run before its terminal event. Expiration should produce
	// one replacement with an independent fence, including on repeated sweeps.
	lost, err := (Submissions{Pool: pool, Artifacts: store}).Submit(ctx, user, contest, cp, "posix_sh", []byte("exec cat\n"))
	must(err)
	var lostAttempt, lostRun string
	must(pool.QueryRow(ctx, `SELECT id,run_id FROM judging_attempts WHERE submission_id=$1`, lost.ID).Scan(&lostAttempt, &lostRun))
	claim := &pb.ClaimRequest{AttemptId: lostAttempt, RunId: lostRun, InvokerId: invoker, ProcessEpoch: uuid.NewString()}
	_, err = control.Heartbeat(ctx, &pb.HeartbeatRequest{InvokerId: invoker, ProtocolVersion: 1, Capacity: 1, ExecutionFormats: []string{"standard"}})
	must(err)
	grant, err := control.Claim(ctx, claim)
	must(err)
	_, err = pool.Exec(ctx, `UPDATE judging_attempts SET lease_until=now()-interval '1 second' WHERE id=$1`, lostAttempt)
	must(err)
	must(recoverExpired(ctx, pool))
	must(retryExpiredStandard(ctx, pool))
	must(retryExpiredStandard(ctx, pool))
	var replacement, replacementRun, reason, parent string
	must(pool.QueryRow(ctx, `SELECT id,run_id,trigger_reason,retry_of FROM judging_attempts WHERE submission_id=$1 AND attempt_number=2`, lost.ID).Scan(&replacement, &replacementRun, &reason, &parent))
	if replacement == lostAttempt || replacementRun == lostRun || reason != "lease_recovery" || parent != lostAttempt {
		t.Fatalf("invalid replacement %s/%s %s %s", replacement, replacementRun, reason, parent)
	}
	_, err = control.RenewLease(ctx, &pb.RunLease{AttemptId: lostAttempt, RunId: lostRun, InvokerId: invoker, ProcessEpoch: claim.ProcessEpoch, FencingToken: grant.FencingToken})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("old attempt renewed after replacement: %v", err)
	}
	must(dispatch(ctx, pool, js))
	service := command("serve-standard")
	var serviceLog bytes.Buffer
	service.Stdout, service.Stderr = &serviceLog, &serviceLog
	must(service.Start())
	serviceDone := make(chan error, 1)
	go func() { serviceDone <- service.Wait() }()
	defer service.Process.Kill()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	donePath := filepath.Join(state, "standard-runs", replacementRun, "done")
	for {
		if _, err := os.Stat(donePath); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		select {
		case err := <-serviceDone:
			t.Fatalf("service exited before replacement completed: %v\n%s", err, &serviceLog)
		case <-ctx.Done():
			<-serviceDone
			t.Fatalf("service did not complete replacement: %v\n%s", ctx.Err(), &serviceLog)
		case <-ticker.C:
		}
	}
	must(service.Process.Signal(syscall.SIGTERM))
	if err := <-serviceDone; err != nil {
		t.Fatalf("service shutdown: %v\n%s", err, &serviceLog)
	}
	must(pool.QueryRow(ctx, `SELECT status,verdict,base_score::text FROM judging_attempts WHERE id=$1`, replacement).Scan(&result, &verdict, &score))
	if result != "finished" || verdict != "accepted" || score != "1.000000" {
		t.Fatalf("replacement results: %q %q %q", result, verdict, score)
	}
	if entries, err := os.ReadDir(filepath.Join(state, "container-journal-v1")); err == nil && len(entries) != 0 {
		t.Fatalf("worker left %d resource ownership records", len(entries))
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

type standardCrashControl struct {
	*ArtifactControl
	committed chan *pb.Event
	replays   atomic.Int32
	dropped   atomic.Bool
}

func (s *standardCrashControl) Report(ctx context.Context, event *pb.Event) (*pb.Acknowledgement, error) {
	ack, err := s.ArtifactControl.Report(ctx, event)
	if err != nil || event.Kind != pb.Event_RUN_FINISHED {
		return ack, err
	}
	if ack.Duplicate {
		s.replays.Add(1)
		return ack, nil
	}
	if s.dropped.Swap(true) {
		return ack, nil
	}
	s.committed <- proto.CloneOf(event)
	<-ctx.Done()
	return nil, ctx.Err()
}
