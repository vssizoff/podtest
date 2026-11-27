package judging

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"podtest/internal/db"
	pb "podtest/internal/invokerpb"
	"podtest/internal/problem"
)

func TestExecutionAdmission(t *testing.T) {
	url := os.Getenv("PODTEST_ADMISSION_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("requires a disposable PostgreSQL database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	pool, err := pgxpool.New(ctx, url)
	must(err)
	defer pool.Close()
	must(db.Migrate(ctx, pool))

	var user, contest, task, bundle string
	must(pool.QueryRow(ctx, `WITH u AS (INSERT INTO users(display_name) VALUES('Admission') RETURNING id),
		c AS (INSERT INTO contests(title,created_by,status) SELECT 'Admission',id,'open' FROM u RETURNING id),
		p AS (INSERT INTO problems(title,created_by) SELECT 'Admission',id FROM u RETURNING id),
		part AS (INSERT INTO contest_participants(contest_id,user_id) SELECT c.id,u.id FROM c,u),
		settings AS (INSERT INTO contest_settings(contest_id) SELECT id FROM c),
		b AS (INSERT INTO artifacts(storage_key,digest,size,media_type,kind)
			VALUES(gen_random_uuid()::text,repeat('0',64),0,'application/octet-stream','fixture') RETURNING id)
		SELECT u.id,c.id,p.id,b.id FROM u,c,p,b`).Scan(&user, &contest, &task, &bundle))

	subs := Submissions{Pool: pool} // Reject before accessing object storage.
	problems := problem.Manager{Pool: pool}
	var legacySubmission, legacyAttempt string
	for i, fixture := range []struct {
		format, manifest string
		available        bool
	}{
		{"standard", `{"package_format":"polygon_full_zip"}`, false},
		{"scripted_lua", `{"package_format":"ustar_v1","execution_ready":false}`, false},
		{"standard", `{"package_format":"ustar_v1","execution_ready":true}`, true},
	} {
		var revision string
		must(pool.QueryRow(ctx, `WITH r AS (INSERT INTO problem_revisions(problem_id,revision_number,execution_format,manifest,bundle_artifact_id,digest)
			VALUES($1,$2,$3,$4,$5,repeat('0',64)) RETURNING id),
			st AS (INSERT INTO problem_statements(revision_id,locale,source,rendered_html,renderer_version,digest)
				SELECT id,'en','fixture','fixture','1',repeat('0',64) FROM r),
			lang AS (INSERT INTO problem_revision_languages(revision_id,language_id)
				SELECT r.id,l.id FROM r,languages l WHERE l.code='python3') SELECT id FROM r`, task, i+1, fixture.format, fixture.manifest, bundle).Scan(&revision))
		_, err := pool.Exec(ctx, `UPDATE problem_revisions SET status='published',published_at=now() WHERE id=$1`, revision)
		must(err)
		var cp string
		must(pool.QueryRow(ctx, `INSERT INTO contest_problems(contest_id,problem_id,problem_revision_id,position,max_score)
			VALUES($1,$2,$3,$4,1) RETURNING id`, contest, task, revision, i).Scan(&cp))
		view, err := problems.Task(ctx, contest, cp)
		must(err)
		if view.ExecutionAvailable != fixture.available {
			t.Fatalf("%s %s: execution_available=%t", fixture.format, fixture.manifest, view.ExecutionAvailable)
		}
		if !fixture.available {
			if _, err := subs.Submit(ctx, user, contest, cp, "python3", []byte("print(1)")); !errors.Is(err, ErrExecutionUnavailable) {
				t.Fatalf("expected execution unavailable, got %v", err)
			}
			items, err := subs.List(ctx, user, contest, cp)
			must(err)
			if len(items) != 0 {
				t.Fatal("rejected request created a submission")
			}
		}

		switch i {
		case 0:
			must(pool.QueryRow(ctx, `INSERT INTO submissions(contest_problem_id,participant_id,contest_id,problem_id,submitted_by,problem_revision_id,language_id,source_artifact_id,digest)
				SELECT $1,p.id,p.contest_id,$2,p.user_id,$3,l.id,$4,repeat('0',64)
				FROM contest_participants p,languages l WHERE p.contest_id=$5 AND l.code='python3' RETURNING id`, cp, task, revision, bundle, contest).Scan(&legacySubmission))
			must(db.Tx(ctx, pool, func(tx pgx.Tx) error {
				var participant string
				if err := tx.QueryRow(ctx, `SELECT participant_id FROM submissions WHERE id=$1`, legacySubmission).Scan(&participant); err != nil {
					return err
				}
				var err error
				legacyAttempt, err = createAttempt(ctx, tx, legacySubmission, contest, participant, revision, "1", user, "fixture", nil)
				return err
			}))
			if _, err := subs.Rejudge(ctx, user, legacySubmission, ""); !errors.Is(err, ErrExecutionUnavailable) {
				t.Fatalf("expected rejudge to reject diagnostic revision, got %v", err)
			}
			if _, err := subs.RejudgePinned(ctx, user, legacySubmission, ""); !errors.Is(err, ErrExecutionUnavailable) {
				t.Fatalf("expected pinned rejudge to reject diagnostic revision, got %v", err)
			}
		case 2:
			ready, err := subs.Rejudge(ctx, user, legacySubmission, revision)
			must(err)
			must(failUnassigned(ctx, pool, true, "Task revision is not available for execution"))
			var status string
			must(pool.QueryRow(ctx, `SELECT status FROM judging_attempts WHERE id=$1`, ready).Scan(&status))
			if status != "queued" {
				t.Fatalf("ready attempt has status %q", status)
			}
			publisher := &recordingPublisher{}
			must(dispatch(ctx, pool, publisher))
			if publisher.calls != 1 {
				t.Fatalf("expected one job to be dispatched, got %d", publisher.calls)
			}
			var published bool
			must(pool.QueryRow(ctx, `SELECT published_at IS NOT NULL FROM outbox_events WHERE dedup_key=$1`, ready).Scan(&published))
			if !published {
				t.Fatal("ready Standard attempt was not dispatched")
			}
			checkProgressAndCancellation(t, ctx, pool, user, legacySubmission, revision, ready, bundle)
		}
	}
	var closed bool
	must(pool.QueryRow(ctx, `SELECT j.status='error' AND j.verdict='testing_error' AND j.base_score IS NULL
		AND q.status='done' AND e.kind='attempt_finished'
		FROM judging_attempts j JOIN queue_entries q ON q.attempt_id=j.id
		JOIN judging_events e ON e.attempt_id=j.id WHERE j.id=$1`, legacyAttempt).Scan(&closed))
	if !closed {
		t.Fatal("legacy attempt was not closed with a testing error")
	}
}

func checkProgressAndCancellation(t *testing.T, ctx context.Context, pool *pgxpool.Pool, actor, submission, revision, queued, artifact string) {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	subs := Submissions{Pool: pool}
	control := ArtifactControl{Pool: pool}
	invoker, epoch := uuid.NewString(), uuid.NewString()
	cert := &x509.Certificate{Subject: pkix.Name{CommonName: invoker}}
	ctx = peer.NewContext(ctx, &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{cert}}}}})
	for range 2 {
		pending, err := subs.Cancel(ctx, actor, submission, queued)
		must(err)
		if pending {
			t.Fatal("pre-claim cancellation should complete immediately")
		}
	}
	var queuedRun string
	must(pool.QueryRow(ctx, `SELECT j.run_id FROM judging_attempts j JOIN queue_entries q ON q.attempt_id=j.id
		WHERE j.id=$1 AND j.status='cancelled' AND q.status='done' AND j.last_sequence=1`, queued).Scan(&queuedRun))
	_, err := control.Claim(ctx, &pb.ClaimRequest{InvokerId: invoker, AttemptId: queued, RunId: queuedRun, ProcessEpoch: epoch})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("cancelled job could be claimed: %v", err)
	}

	attempt, err := subs.Rejudge(ctx, actor, submission, revision)
	must(err)
	_, err = pool.Exec(ctx, `INSERT INTO invokers(id,protocol_version,capabilities,capacity,load) VALUES($1,1,ARRAY['standard'],1,0)`, invoker)
	must(err)
	fence := make([]byte, 32)
	var run string
	must(pool.QueryRow(ctx, `UPDATE judging_attempts SET status='assigned',invoker_id=$2,process_epoch=$3,fencing_token=$4,
		lease_until=clock_timestamp()+interval '2 minutes' WHERE id=$1 RETURNING run_id`, attempt, invoker, epoch, fence).Scan(&run))
	_, err = pool.Exec(ctx, `UPDATE queue_entries SET status='assigned' WHERE attempt_id=$1`, attempt)
	must(err)
	_, err = pool.Exec(ctx, `INSERT INTO attempt_artifacts(attempt_id,artifact_id,kind) VALUES($1,$2,'build_log')`, attempt, artifact)
	must(err)
	event := func(sequence uint64, kind pb.Event_Kind, testID string) *pb.Event {
		id := uuid.New()
		return &pb.Event{EventId: id[:], AttemptId: attempt, RunId: run, SubmissionId: submission, InvokerId: invoker,
			Sequence: sequence, SchemaVersion: 1, OccurredAtUnixMillis: time.Now().UnixMilli(), FencingToken: fence, Kind: kind, TestId: testID}
	}
	for i, kind := range []pb.Event_Kind{pb.Event_RUN_STARTED, pb.Event_BUILD_STARTED, pb.Event_BUILD_FINISHED} {
		_, err := control.Report(ctx, event(uint64(i+1), kind, ""))
		must(err)
	}
	started := event(4, pb.Event_TEST_STARTED, "test-1")
	_, err = control.Report(ctx, started)
	must(err)
	ack, err := control.Report(ctx, started)
	must(err)
	if !ack.Duplicate {
		t.Fatal("event replay should be acknowledged as duplicate")
	}
	for _, visibility := range []string{"hidden", "summary", "tests", "manager"} {
		items, err := subs.Attempts(ctx, submission, visibility)
		must(err)
		latest := items[0]
		showProgress := visibility == "manager"
		if latest.Status != "running" || (latest.Phase == "testing") != showProgress || (len(latest.ActiveTests) != 0) != showProgress || (len(latest.Artifacts) != 0) != (visibility == "manager") {
			t.Fatalf("unexpected attempt fields for %s: %+v", visibility, latest)
		}
	}
	finishedTest := event(5, pb.Event_TEST_FINISHED, "test-1")
	score := "1"
	finishedTest.Score, finishedTest.Verdict = &score, "accepted"
	_, err = control.Report(ctx, finishedTest)
	must(err)
	items, err := subs.Attempts(ctx, submission, "tests")
	must(err)
	if len(items[0].ActiveTests) != 0 {
		t.Fatal("finished test remains active")
	}
	cancelled := event(6, pb.Event_RUN_CANCELLED, "")
	if _, err := control.Report(ctx, cancelled); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unsolicited cancellation accepted: %v", err)
	}
	for range 2 {
		pending, err := subs.Cancel(ctx, actor, submission, attempt)
		must(err)
		if !pending {
			t.Fatal("active cancellation should be pending")
		}
	}
	items, err = subs.Attempts(ctx, submission, "manager")
	must(err)
	if !items[0].CancelPending || items[0].CanCancel {
		t.Fatalf("unexpected pending cancellation view: %+v", items[0])
	}
	lease := &pb.RunLease{InvokerId: invoker, AttemptId: attempt, RunId: run, ProcessEpoch: epoch, FencingToken: fence}
	var expiry int64
	must(pool.QueryRow(ctx, `UPDATE judging_attempts SET lease_until=clock_timestamp()+interval '1 minute'
		WHERE id=$1 RETURNING floor(extract(epoch FROM lease_until))::bigint`, attempt).Scan(&expiry))
	for range 2 {
		renewed, err := control.RenewLease(ctx, lease)
		must(err)
		if !renewed.CancelRequested {
			t.Fatal("renewal did not return pending cancellation")
		}
		if renewed.LeaseExpiresUnix != expiry {
			t.Fatal("pending cancellation extended the lease")
		}
	}
	lease.FencingToken = []byte("stale")
	if _, err := control.RenewLease(ctx, lease); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("stale fence received a renewal: %v", err)
	}
	_, err = control.Report(ctx, cancelled)
	must(err)
	ack, err = control.Report(ctx, cancelled)
	must(err)
	if !ack.Duplicate {
		t.Fatal("cancellation replay was not acknowledged")
	}
	items, err = subs.Attempts(ctx, submission, "manager")
	must(err)
	if items[0].Status != "cancelled" || items[0].CancelPending || items[0].CanCancel {
		t.Fatalf("unexpected cancelled attempt view: %+v", items[0])
	}
	if _, err := control.Report(ctx, event(7, pb.Event_TEST_STARTED, "test-2")); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("terminal attempt accepted progress: %v", err)
	}
	lost, err := subs.Rejudge(ctx, actor, submission, revision)
	must(err)
	_, err = pool.Exec(ctx, `UPDATE judging_attempts SET status='assigned',lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, lost)
	must(err)
	_, err = pool.Exec(ctx, `UPDATE queue_entries SET status='assigned' WHERE attempt_id=$1`, lost)
	must(err)
	_, err = subs.Cancel(ctx, actor, submission, lost)
	must(err)
	must(recoverExpired(ctx, pool))
	must(retryExpiredStandard(ctx, pool))
	items, err = subs.Attempts(ctx, submission, "manager")
	must(err)
	if items[0].ID != lost || items[0].Status != "cancelled" {
		t.Fatalf("expired cancellation recovered incorrectly: %+v", items[0])
	}
}
