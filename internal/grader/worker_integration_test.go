package grader

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"podtest/internal/db"
)

func TestWorkerPublication(t *testing.T) {
	dsn, image := os.Getenv("PODTEST_GRADER_TEST_DATABASE_URL"), os.Getenv("GRADER_IMAGE_ID")
	if dsn == "" || image == "" {
		t.Skip("requires disposable PostgreSQL and a local Python Podman image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	pool, err := pgxpool.New(ctx, dsn)
	must(err)
	defer pool.Close()
	must(db.Migrate(ctx, pool))
	m := Manager{Pool: pool}
	var actor, contest, cp, participant, attempt, revision, bundle string
	must(pool.QueryRow(ctx, `WITH u AS (INSERT INTO users(display_name) VALUES('Grader'),('Missing') RETURNING id,display_name),
		c AS (INSERT INTO contests(title,created_by) SELECT 'Grader',id FROM u WHERE display_name='Grader' RETURNING id,created_by),
		p AS (INSERT INTO problems(title,created_by) SELECT 'Grader',created_by FROM c RETURNING id),
		r AS (INSERT INTO problem_revisions(problem_id,revision_number,execution_format,grader_mode) SELECT id,1,'standard','automatic_submission' FROM p RETURNING id,problem_id),
		l AS (INSERT INTO problem_revision_languages(revision_id,language_id) SELECT r.id,languages.id FROM r,languages WHERE code='python3'),
		st AS (INSERT INTO problem_statements(revision_id,locale,source,rendered_html,renderer_version,digest) SELECT id,'en','fixture','fixture','1',repeat('0',64) FROM r),
		part AS (INSERT INTO contest_participants(contest_id,user_id) SELECT c.id,u.id FROM c,u RETURNING *),
		b AS (INSERT INTO artifacts(storage_key,digest,size,media_type,kind) VALUES(gen_random_uuid()::text,repeat('0',64),0,'application/octet-stream','fixture') RETURNING id)
		SELECT c.created_by,c.id,r.id,b.id FROM c,r,b`).Scan(&actor, &contest, &revision, &bundle))
	_, err = pool.Exec(ctx, `UPDATE problem_revisions SET status='published',published_at=now(),digest=repeat('0',64),bundle_artifact_id=$2 WHERE id=$1`, revision, bundle)
	must(err)
	must(pool.QueryRow(ctx, `WITH cp AS (INSERT INTO contest_problems(contest_id,problem_id,problem_revision_id,position,max_score)
		SELECT $1,r.problem_id,r.id,n,10 FROM problem_revisions r,generate_series(0,1) n WHERE r.id=$2 RETURNING *),
		s AS (INSERT INTO submissions(contest_problem_id,participant_id,contest_id,problem_id,submitted_by,problem_revision_id,language_id,source_artifact_id,digest)
		SELECT cp.id,part.id,cp.contest_id,cp.problem_id,part.user_id,l.revision_id,l.language_id,$3,repeat('0',64)
		FROM cp JOIN contest_participants part ON part.contest_id=cp.contest_id JOIN problem_revision_languages l ON l.revision_id=cp.problem_revision_id WHERE part.user_id=$4 RETURNING *),
		j AS (INSERT INTO judging_attempts(submission_id,attempt_number,target_problem_revision_id,status,base_score,trigger_reason,max_score)
		SELECT s.id,1,s.problem_revision_id,'finished',CASE position WHEN 0 THEN 7 ELSE 9 END,'fixture',10 FROM s JOIN cp ON cp.id=s.contest_problem_id RETURNING *)
		SELECT cp.id,s.participant_id,j.id FROM cp,s,j WHERE cp.position=0 AND s.contest_problem_id=cp.id AND j.submission_id=s.id`, contest, revision, bundle, actor).Scan(&cp, &participant, &attempt))
	_, err = pool.Exec(ctx, `INSERT INTO grader_triggers(attempt_id,contest_problem_id) VALUES($1,$2)`, attempt, cp)
	must(err)
	run, err := m.claim(ctx)
	must(err)
	if run.ID == "" || run.Mode != "preview" {
		t.Fatalf("automatic trigger did not produce a preview: %+v", run)
	}
	inputs, err := m.SnapshotScores(ctx, run.ID, run.Capability)
	must(err)
	if len(inputs) != 1 || inputs[0].AttemptID == nil || *inputs[0].AttemptID != attempt {
		t.Fatalf("unexpected scope: %+v", inputs)
	}
	if _, err := m.SnapshotScores(ctx, run.ID, "wrong-token"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("invalid token: %v", err)
	}
	must(m.execute(ctx, image, run))
	var published *string
	var status, log string
	var scoresOK bool
	must(pool.QueryRow(ctx, `SELECT g.status,g.log,c.current_score_publication_id,
		(SELECT count(*)=2 AND sum(score)=7 AND max(score) FILTER(WHERE participant_id=$2)=7 FROM grader_scores WHERE grader_run_id=g.id)
		FROM grader_runs g JOIN contests c ON c.id=g.contest_id WHERE g.id=$1`, run.ID, participant).Scan(&status, &log, &published, &scoresOK))
	if status != "completed" || log == "" || published != nil || !scoresOK {
		t.Fatalf("preview: status=%s log=%q published=%v scoresOK=%t", status, log, published, scoresOK)
	}
	id, err := m.Publish(ctx, actor, run.ID)
	must(err)
	manual, _, err := m.Create(ctx, actor, contest, cp, "manual", "preview")
	must(err)
	run, err = m.claim(ctx)
	must(err)
	if run.ID != manual.ID || run.Capability == manual.Capability {
		t.Fatal("manual claim did not replace capability")
	}
	must(m.execute(ctx, image, run))
	must(pool.QueryRow(ctx, `SELECT current_score_publication_id FROM contests WHERE id=$1`, contest).Scan(&published))
	if published == nil || *published != id {
		t.Fatalf("manual preview replaced publication: %v", published)
	}
	newID, err := m.Publish(ctx, actor, run.ID)
	must(err)
	must(pool.QueryRow(ctx, `SELECT current_score_publication_id FROM contests WHERE id=$1`, contest).Scan(&published))
	if newID == id || *published != newID {
		t.Fatal("publication was not switched")
	}
	stale, _, err := m.Create(ctx, actor, contest, cp, "manual", "preview")
	must(err)
	run, err = m.claim(ctx)
	must(err)
	if run.ID != stale.ID {
		t.Fatal("expected new manual claim")
	}
	_, err = pool.Exec(ctx, `UPDATE grader_runs SET capability_expires_at=now()-interval '1 second' WHERE id=$1`, run.ID)
	must(err)
	_, err = m.claim(ctx)
	must(err)
	must(pool.QueryRow(ctx, `SELECT status,log FROM grader_runs WHERE id=$1`, run.ID).Scan(&status, &log))
	if status != "failed" || log != "Worker lease expired" {
		t.Fatalf("expired claim: %s %q", status, log)
	}
	if _, err := m.Publish(ctx, actor, run.ID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expired run publish: %v", err)
	}
}
