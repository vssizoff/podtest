package judging

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"podtest/internal/artifact"
	"podtest/internal/authorization"
	"podtest/internal/db"
	"podtest/internal/problem"
)

var ErrLimit = errors.New("submission limit reached")
var ErrInvalid = errors.New("contest closed, participant inactive, or language unavailable")
var ErrExecutionUnavailable = errors.New("this task revision is not available for execution")

type Submissions struct {
	Pool      *pgxpool.Pool
	Artifacts *artifact.Store
}
type Submission struct {
	ID          string    `json:"id"`
	ContestID   string    `json:"contest_id"`
	ProblemID   string    `json:"contest_problem_id"`
	RevisionID  string    `json:"problem_revision_id"`
	SubmittedAt time.Time `json:"submitted_at"`
	Language    string    `json:"language"`
}
type Attempt struct {
	ID            string            `json:"id"`
	Number        int               `json:"attempt_number"`
	Status        string            `json:"status"`
	Verdict       *string           `json:"verdict,omitempty"`
	Score         *string           `json:"base_score,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	Tests         []Test            `json:"tests,omitempty"`
	Protocol      *Protocol         `json:"protocol,omitempty"`
	Phase         string            `json:"phase,omitempty"`
	ActiveTests   []string          `json:"active_tests,omitempty"`
	Artifacts     []AttemptArtifact `json:"artifacts,omitempty"`
	CanCancel     bool              `json:"can_cancel,omitempty"`
	CancelPending bool              `json:"cancel_pending,omitempty"`
}

type AttemptArtifact struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}
type Test struct {
	ID       string          `json:"test_id"`
	Verdict  string          `json:"verdict"`
	Score    *string         `json:"score,omitempty"`
	Metadata json.RawMessage `json:"checker_metadata,omitempty"`
}

type Protocol struct {
	RevisionID string        `json:"problem_revision_id"`
	StartedAt  *time.Time    `json:"started_at,omitempty"`
	FinishedAt *time.Time    `json:"finished_at,omitempty"`
	Groups     []GroupResult `json:"groups"`
}

type GroupResult struct {
	ID    string `json:"subgroup_id"`
	Score string `json:"score"`
}

func (s Submissions) Submit(ctx context.Context, user, contest, cp, language string, source []byte) (out Submission, err error) {
	if len(source) == 0 || len(source) > 256<<10 || language == "" {
		return out, ErrInvalid
	}
	err = db.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		var maxQueue, limit *int
		var participant, languageID, problemID, revision, score string
		err := tx.QueryRow(ctx, `SELECT cs.max_queued_submissions FROM contests c JOIN contest_settings cs ON cs.contest_id=c.id WHERE c.id=$1 AND c.status='open' AND c.archived_at IS NULL AND (c.starts_at IS NULL OR c.starts_at<=now()) AND (c.ends_at IS NULL OR c.ends_at>now()) FOR UPDATE OF c`, contest).Scan(&maxQueue)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalid
		}
		if err != nil {
			return err
		}
		var runnable bool
		err = tx.QueryRow(ctx, `SELECT cp.problem_id,cp.problem_revision_id,cp.max_score::text,cp.submission_limit,p.id,l.id,`+problem.RunnableRevisionSQL+`
			FROM contest_problems cp JOIN contest_participants p ON p.contest_id=cp.contest_id
			JOIN problem_revisions r ON r.id=cp.problem_revision_id
			JOIN problem_revision_languages x ON x.revision_id=r.id JOIN languages l ON l.id=x.language_id
			WHERE cp.id=$1 AND cp.contest_id=$2 AND p.user_id=$3 AND p.status='active' AND l.code=$4 AND cp.detached_at IS NULL
			FOR SHARE OF cp,p`, cp, contest, user, language).Scan(&problemID, &revision, &score, &limit, &participant, &languageID, &runnable)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalid
		}
		if err != nil {
			return err
		}
		if !runnable {
			return ErrExecutionUnavailable
		}
		if limit != nil {
			var n int
			if err = tx.QueryRow(ctx, `SELECT count(*) FROM submissions WHERE participant_id=$1 AND contest_problem_id=$2`, participant, cp).Scan(&n); err != nil {
				return err
			}
			if n >= *limit {
				return ErrLimit
			}
		}
		ref, err := s.Artifacts.PutTx(ctx, tx, user, "submission-source", "application/octet-stream", source)
		if err != nil {
			return err
		}
		out = Submission{ContestID: contest, ProblemID: cp, RevisionID: revision, Language: language}
		if err = tx.QueryRow(ctx, `INSERT INTO submissions(contest_problem_id,participant_id,contest_id,problem_id,submitted_by,problem_revision_id,language_id,source_artifact_id,digest) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id,submitted_at`, cp, participant, contest, problemID, user, revision, languageID, ref.ID, ref.Digest).Scan(&out.ID, &out.SubmittedAt); err != nil {
			return err
		}
		if _, err = createAttempt(ctx, tx, out.ID, contest, participant, revision, score, user, "submission", maxQueue); err != nil {
			return err
		}
		for _, t := range []authorization.Tuple{{User: "user:" + user, Relation: "owner", Object: "submission:" + out.ID}, {User: "contest:" + contest, Relation: "contest", Object: "submission:" + out.ID}} {
			if err = authorization.Enqueue(ctx, tx, t, false); err != nil {
				return err
			}
		}
		return nil
	})
	return
}
func createAttempt(ctx context.Context, tx pgx.Tx, id, contest, participant, revision, score string, user any, reason string, cap *int) (string, error) {
	status := "queued"
	if cap != nil {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM queue_entries WHERE contest_id=$1 AND participant_id=$2 AND status IN ('queued','assigned','waiting')`, contest, participant).Scan(&n); err != nil {
			return "", err
		}
		if n >= *cap {
			status = "waiting"
		}
	}
	var attempt string
	if err := tx.QueryRow(ctx, `INSERT INTO judging_attempts(submission_id,attempt_number,target_problem_revision_id,status,trigger_actor,trigger_reason,retry_of,max_score) SELECT $1,coalesce((SELECT max(attempt_number) FROM judging_attempts WHERE submission_id=$1),0)+1,$2,$3,$4,$5,(SELECT id FROM judging_attempts WHERE submission_id=$1 ORDER BY attempt_number DESC LIMIT 1),$6::numeric RETURNING id`, id, revision, status, user, reason, score).Scan(&attempt); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO queue_entries(attempt_id,contest_id,participant_id,status) VALUES($1,$2,$3,$4)`, attempt, contest, participant, status); err != nil {
		return "", err
	}
	return attempt, enqueue(ctx, tx, attempt)
}
func enqueue(ctx context.Context, tx pgx.Tx, attempt string) error {
	_, err := tx.Exec(ctx, `INSERT INTO outbox_events(topic,dedup_key,payload)
 SELECT CASE WHEN r.execution_format='scripted_lua' THEN $3 ELSE $2 END,j.id::text,jsonb_build_object('job_id',j.id,'run_id',j.run_id,'judging_attempt_id',j.id,'submission_id',s.id,'contest_id',s.contest_id,'contest_problem_id',s.contest_problem_id,'task_id',s.problem_id,'task_revision_id',r.id,'execution_format',r.execution_format,'execution_format_version',r.format_version,'created_at',to_char(j.created_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),'schema_version',1)
 || CASE WHEN r.execution_format='standard' AND r.manifest->'execution_ready'='true'::jsonb THEN jsonb_build_object('language',l.code) ELSE '{}'::jsonb END
 FROM judging_attempts j JOIN submissions s ON s.id=j.submission_id JOIN languages l ON l.id=s.language_id JOIN problem_revisions r ON r.id=j.target_problem_revision_id WHERE j.id=$1`, attempt, Subject, luaSubject)
	return err
}
func (s Submissions) Rejudge(ctx context.Context, user, id, revision string) (out string, err error) {
	return s.rejudge(ctx, user, id, revision, false)
}

func (s Submissions) RejudgePinned(ctx context.Context, user, id, expectedRevision string) (string, error) {
	return s.rejudge(ctx, user, id, expectedRevision, true)
}

func (s Submissions) rejudge(ctx context.Context, user, id, revision string, pinned bool) (out string, err error) {
	err = db.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		var contest, participant, target, score string
		var maxQueue *int
		var runnable bool
		if err := tx.QueryRow(ctx, `SELECT s.contest_id,s.participant_id,r.id,cp.max_score::text,cs.max_queued_submissions,`+problem.RunnableRevisionSQL+`
			FROM submissions s JOIN contest_problems cp ON cp.id=s.contest_problem_id JOIN contests c ON c.id=s.contest_id
			JOIN contest_settings cs ON cs.contest_id=c.id
			JOIN problem_revisions r ON r.id=CASE WHEN $3 THEN cp.problem_revision_id ELSE coalesce(NULLIF($2,'')::uuid,s.problem_revision_id) END AND r.problem_id=s.problem_id
			JOIN problem_revision_languages x ON x.revision_id=r.id AND x.language_id=s.language_id
			WHERE s.id=$1 AND r.status='published' AND c.archived_at IS NULL AND (NOT $3 OR cp.detached_at IS NULL) FOR UPDATE OF c FOR SHARE OF cp`, id, revision, pinned).Scan(&contest, &participant, &target, &score, &maxQueue, &runnable); err != nil {
			return err
		}
		if !runnable || (pinned && revision != "" && revision != target) {
			return ErrExecutionUnavailable
		}
		var err error
		out, err = createAttempt(ctx, tx, id, contest, participant, target, score, user, "rejudge", maxQueue)
		if err != nil {
			return err
		}
		return db.Audit(ctx, tx, user, "submission.rejudge", id)
	})
	return
}

const submissionSelect = `SELECT s.id,s.contest_id,s.contest_problem_id,s.problem_revision_id,s.submitted_at,l.code FROM submissions s JOIN languages l ON l.id=s.language_id `

func (s Submissions) Get(ctx context.Context, id string) (out Submission, err error) {
	err = s.Pool.QueryRow(ctx, submissionSelect+`WHERE s.id=$1`, id).Scan(&out.ID, &out.ContestID, &out.ProblemID, &out.RevisionID, &out.SubmittedAt, &out.Language)
	return
}
func (s Submissions) List(ctx context.Context, user, contest, cp string) ([]Submission, error) {
	return s.list(ctx, submissionSelect+`WHERE s.submitted_by=$1 AND (NULLIF($2,'')::uuid IS NULL OR s.contest_id=NULLIF($2,'')::uuid) AND (NULLIF($3,'')::uuid IS NULL OR s.contest_problem_id=NULLIF($3,'')::uuid)`, user, contest, cp)
}

func (s Submissions) ListTask(ctx context.Context, contest, cp, after string) ([]Submission, string, error) {
	rows, err := s.Pool.Query(ctx, submissionSelect+`WHERE s.contest_id=$1 AND s.contest_problem_id=$2
		AND (NULLIF($3,'') IS NULL OR (s.submitted_at,s.id)<(SELECT submitted_at,id FROM submissions WHERE id=NULLIF($3,'')::uuid AND contest_id=$1 AND contest_problem_id=$2))
		ORDER BY s.submitted_at DESC,s.id DESC LIMIT 101`, contest, cp, after)
	if err != nil {
		return nil, "", err
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Submission])
	if items == nil {
		items = []Submission{}
	}
	next := ""
	if len(items) == 101 {
		items = items[:100]
		next = items[99].ID
	}
	return items, next, err
}

func (s Submissions) list(ctx context.Context, query string, args ...any) ([]Submission, error) {
	rows, err := s.Pool.Query(ctx, query+` ORDER BY s.submitted_at DESC,s.id LIMIT 100`, args...)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Submission])
	if out == nil {
		out = []Submission{}
	}
	return out, err
}
func (s Submissions) Attempts(ctx context.Context, id, visibility string) ([]Attempt, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id,attempt_number,status,CASE WHEN $2 IN ('manager','summary') THEN verdict END,
		CASE WHEN $2 IN ('manager','summary') THEN base_score::text END,created_at,
		$2='manager' AND status IN ('assigned','running') AND cancel_requested_at IS NOT NULL,
		target_problem_revision_id,started_at,finished_at
		FROM judging_attempts WHERE submission_id=$1 ORDER BY attempt_number DESC LIMIT 100`, id, visibility)
	if err != nil {
		return nil, err
	}
	out := []Attempt{}
	for rows.Next() {
		var a Attempt
		var protocol Protocol
		if err = rows.Scan(&a.ID, &a.Number, &a.Status, &a.Verdict, &a.Score, &a.CreatedAt, &a.CancelPending, &protocol.RevisionID, &protocol.StartedAt, &protocol.FinishedAt); err != nil {
			rows.Close()
			return nil, err
		}
		if visibility == "manager" {
			a.Protocol = &protocol
		}
		out = append(out, a)
	}
	rows.Close()
	if rows.Err() != nil {
		return nil, rows.Err()
	}
	if visibility == "tests" || visibility == "manager" {
		for i := range out {
			rows, err := s.Pool.Query(ctx, `SELECT test_id,verdict,CASE WHEN $2 THEN score::text END,
				CASE WHEN $2 THEN checker_metadata END FROM test_results WHERE attempt_id=$1 ORDER BY test_id`, out[i].ID, visibility == "manager")
			if err != nil {
				return nil, err
			}
			out[i].Tests, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Test])
			if err != nil {
				return nil, err
			}
			if visibility == "manager" && out[i].Status == "running" {
				if err := s.loadProgress(ctx, &out[i]); err != nil {
					return nil, err
				}
			}
			if visibility == "manager" {
				groups, err := s.Pool.Query(ctx, `SELECT subgroup_id,score::text FROM subgroup_results WHERE attempt_id=$1 ORDER BY subgroup_id`, out[i].ID)
				if err != nil {
					return nil, err
				}
				out[i].Protocol.Groups, err = pgx.CollectRows(groups, pgx.RowToStructByPos[GroupResult])
				if err != nil {
					return nil, err
				}
				out[i].CanCancel = !out[i].CancelPending && (out[i].Status == "waiting" || out[i].Status == "queued" || out[i].Status == "assigned" || out[i].Status == "running")
				rows, err := s.Pool.Query(ctx, `SELECT x.artifact_id,x.kind FROM attempt_artifacts x JOIN artifacts a ON a.id=x.artifact_id
					WHERE x.attempt_id=$1 ORDER BY a.created_at,x.artifact_id LIMIT 1000`, out[i].ID)
				if err != nil {
					return nil, err
				}
				out[i].Artifacts, err = pgx.CollectRows(rows, pgx.RowToStructByPos[AttemptArtifact])
				if err != nil {
					return nil, err
				}
			}
		}
	}
	return out, nil
}
