package grader

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"podtest/internal/artifact"
	"podtest/internal/db"
)

var ErrInvalid = errors.New("invalid grader snapshot or scores")
var decimal = regexp.MustCompile(`^(0|[1-9][0-9]{0,11})(\.[0-9]{1,6})?$`)

type Manager struct {
	Pool      *pgxpool.Pool
	Artifacts *artifact.Store
}
type Input struct {
	SubmissionID  string  `json:"submission_id"`
	AttemptID     *string `json:"judging_attempt_id"`
	ParticipantID string  `json:"participant_id"`
}
type Run struct {
	ID             string    `json:"id"`
	ContestID      string    `json:"contest_id"`
	ProblemID      string    `json:"contest_problem_id"`
	SnapshotID     string    `json:"snapshot_id"`
	SnapshotDigest string    `json:"snapshot_digest"`
	Mode           string    `json:"mode"`
	Status         string    `json:"status"`
	Capability     string    `json:"-"`
	Expires        time.Time `json:"-"`
}
type Score struct {
	ParticipantID    string          `json:"participant_id"`
	ContestProblemID string          `json:"contest_problem_id"`
	Value            string          `json:"score"`
	Metadata         json.RawMessage `json:"metadata,omitempty"`
}
type ScorePayload struct {
	Scores []Score `json:"scores"`
	Log    string  `json:"log,omitempty"`
}
type Scope struct {
	Participants []string `json:"participant_ids"`
	Inputs       []Input  `json:"inputs"`
}

func (m Manager) Create(ctx context.Context, actor, contest, cp, trigger, mode string) (out Run, scope Scope, err error) {
	if (trigger != "automatic_submission" && trigger != "manual") || (mode != "preview" && mode != "publish") {
		return out, scope, ErrInvalid
	}
	err = db.Tx(ctx, m.Pool, func(tx pgx.Tx) error {
		out, scope, err = m.createTx(ctx, tx, actor, contest, cp, trigger, mode)
		return err
	})
	return
}

func (m Manager) createTx(ctx context.Context, tx pgx.Tx, actor, contest, cp, trigger, mode string) (out Run, scope Scope, err error) {
	err = func() error {
		var max string
		var graderMode *string
		if err := tx.QueryRow(ctx, `SELECT cp.max_score::text,r.grader_mode FROM contest_problems cp JOIN problem_revisions r ON r.id=cp.problem_revision_id WHERE cp.id=$1 AND cp.contest_id=$2 AND cp.detached_at IS NULL FOR UPDATE OF cp`, cp, contest).Scan(&max, &graderMode); err != nil {
			return err
		}
		if trigger == "automatic_submission" && (graderMode == nil || *graderMode != "automatic_submission") {
			return ErrInvalid
		}
		if graderMode == nil {
			return ErrInvalid
		}
		participantRows, err := tx.Query(ctx, `SELECT id FROM contest_participants WHERE contest_id=$1 AND status='active' ORDER BY id`, contest)
		if err != nil {
			return err
		}
		participants, err := pgx.CollectRows(participantRows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		scope.Participants = participants
		rows, err := tx.Query(ctx, `SELECT s.id,j.id,s.participant_id FROM submissions s JOIN judging_attempts j ON j.submission_id=s.id
			WHERE s.contest_problem_id=$1 AND s.participant_id=ANY($2::uuid[])
			AND j.id=(SELECT a.id FROM judging_attempts a WHERE a.submission_id=s.id ORDER BY a.attempt_number DESC LIMIT 1) AND j.status IN ('finished','error','timeout') ORDER BY s.id,j.id LIMIT 100001`, cp, participants)
		if err != nil {
			return err
		}
		for rows.Next() {
			var in Input
			if err := rows.Scan(&in.SubmissionID, &in.AttemptID, &in.ParticipantID); err != nil {
				rows.Close()
				return err
			}
			scope.Inputs = append(scope.Inputs, in)
		}
		rows.Close()
		if rows.Err() != nil {
			return rows.Err()
		}
		if len(scope.Inputs) > 100000 {
			return ErrInvalid
		}
		canonical := append([]Input(nil), scope.Inputs...)
		sort.Slice(canonical, func(i, j int) bool { return canonical[i].SubmissionID < canonical[j].SubmissionID })
		snap, err := json.Marshal(struct {
			Contest      string
			Problem      string
			Participants []string
			Inputs       []Input
		}{contest, cp, scope.Participants, canonical})
		if err != nil {
			return err
		}
		sum := sha256.Sum256(snap)
		raw := make([]byte, 32)
		if _, err = rand.Read(raw); err != nil {
			return err
		}
		token := hex.EncodeToString(raw)
		tokenHash := sha256.Sum256([]byte(token))
		out = Run{ID: uuid.NewString(), ContestID: contest, ProblemID: cp, SnapshotID: uuid.NewString(), SnapshotDigest: hex.EncodeToString(sum[:]), Mode: mode, Status: "pending", Capability: token, Expires: time.Now().Add(10 * time.Minute)}
		if err = tx.QueryRow(ctx, `INSERT INTO grader_runs(id,contest_id,contest_problem_id,problem_revision_id,trigger_type,trigger_actor,mode,status,snapshot_id,snapshot_digest,max_score,capability_hash,capability_expires_at) SELECT $1,$2,$3,problem_revision_id,$4,NULLIF($5,'')::uuid,$6,'pending',$7,$8,$9::numeric,$10,$11 FROM contest_problems WHERE id=$3 RETURNING id`, out.ID, contest, cp, trigger, actor, mode, out.SnapshotID, out.SnapshotDigest, max, tokenHash[:], out.Expires).Scan(&out.ID); err != nil {
			return err
		}
		for _, p := range scope.Participants {
			if _, err = tx.Exec(ctx, `INSERT INTO grader_participants(grader_run_id,participant_id) VALUES($1,$2)`, out.ID, p); err != nil {
				return err
			}
		}
		for _, in := range scope.Inputs {
			if _, err = tx.Exec(ctx, `INSERT INTO grader_inputs(grader_run_id,participant_id,submission_id,judging_attempt_id) SELECT $1,s.participant_id,s.id,$3 FROM submissions s WHERE s.id=$2`, out.ID, in.SubmissionID, in.AttemptID); err != nil {
				return err
			}
		}
		return db.Audit(ctx, tx, actor, "grader.create", out.ID)
	}()
	return
}

func (m Manager) Snapshot(ctx context.Context, run, token string) (Run, Scope, error) {
	h := sha256.Sum256([]byte(token))
	var out Run
	err := m.Pool.QueryRow(ctx, `SELECT id,contest_id,contest_problem_id,snapshot_id,snapshot_digest,mode,status,capability_expires_at FROM grader_runs WHERE id=$1 AND capability_hash=$2 AND capability_expires_at>now()`, run, h[:]).Scan(&out.ID, &out.ContestID, &out.ProblemID, &out.SnapshotID, &out.SnapshotDigest, &out.Mode, &out.Status, &out.Expires)
	if err != nil {
		return Run{}, Scope{}, err
	}
	rows, err := m.Pool.Query(ctx, `SELECT participant_id FROM grader_participants WHERE grader_run_id=$1 ORDER BY participant_id`, run)
	if err != nil {
		return Run{}, Scope{}, err
	}
	participants, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return Run{}, Scope{}, err
	}
	rows, err = m.Pool.Query(ctx, `SELECT submission_id,judging_attempt_id,participant_id FROM grader_inputs WHERE grader_run_id=$1 ORDER BY submission_id`, run)
	if err != nil {
		return Run{}, Scope{}, err
	}
	defer rows.Close()
	inputs := []Input{}
	for rows.Next() {
		var in Input
		if err := rows.Scan(&in.SubmissionID, &in.AttemptID, &in.ParticipantID); err != nil {
			return Run{}, Scope{}, err
		}
		inputs = append(inputs, in)
	}
	return out, Scope{participants, inputs}, rows.Err()
}

func (m Manager) Submit(ctx context.Context, run, token string, p ScorePayload) error {
	if len(p.Log) > 1<<20 {
		return ErrInvalid
	}
	out, scope, err := m.Snapshot(ctx, run, token)
	if err != nil {
		return err
	}
	var max string
	if err = m.Pool.QueryRow(ctx, `SELECT max_score::text FROM grader_runs WHERE id=$1`, run).Scan(&max); err != nil {
		return err
	}
	maxValue, ok := new(big.Rat).SetString(max)
	if !ok {
		return ErrInvalid
	}
	allowedP := map[string]bool{}
	for _, id := range scope.Participants {
		allowedP[id] = true
	}
	seen := map[string]bool{}
	if len(p.Scores) != len(allowedP) {
		return ErrInvalid
	}
	for _, s := range p.Scores {
		v, ok := new(big.Rat).SetString(s.Value)
		if !ok || !decimal.MatchString(s.Value) || v.Sign() < 0 || v.Cmp(maxValue) > 0 || !allowedP[s.ParticipantID] || s.ContestProblemID != out.ProblemID || seen[s.ParticipantID] {
			return ErrInvalid
		}
		if len(s.Metadata) > 8192 {
			return ErrInvalid
		}
		var meta any
		if len(s.Metadata) > 0 {
			if json.Unmarshal(s.Metadata, &meta) != nil {
				return ErrInvalid
			}
			if !validJSON(meta) {
				return ErrInvalid
			}
		}
		seen[s.ParticipantID] = true
	}
	return db.Tx(ctx, m.Pool, func(tx pgx.Tx) error {
		var status string
		h := sha256.Sum256([]byte(token))
		if err := tx.QueryRow(ctx, `SELECT status FROM grader_runs WHERE id=$1 AND capability_hash=$2 AND capability_expires_at>clock_timestamp() FOR UPDATE`, run, h[:]).Scan(&status); err != nil {
			return err
		}
		if status != "pending" && status != "running" {
			return ErrInvalid
		}
		if _, err := tx.Exec(ctx, `UPDATE grader_runs SET status='completed',finished_at=now(),log=$2 WHERE id=$1`, run, p.Log); err != nil {
			return err
		}
		for _, s := range p.Scores {
			meta := s.Metadata
			if len(meta) == 0 {
				meta = []byte(`{}`)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO grader_scores(grader_run_id,participant_id,contest_problem_id,score,metadata) VALUES($1,$2,$3,$4::numeric,$5)`, run, s.ParticipantID, out.ProblemID, s.Value, meta); err != nil {
				return err
			}
		}
		return nil
	})
}
func validJSON(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if k == "__proto__" || k == "constructor" || !validJSON(val) {
				return false
			}
		}
	case []any:
		for _, val := range x {
			if !validJSON(val) {
				return false
			}
		}
	}
	return true
}

func (m Manager) Publish(ctx context.Context, actor, run string) (string, error) {
	var publication string
	err := db.Tx(ctx, m.Pool, func(tx pgx.Tx) error {
		var contest, cp, status, mode string
		if err := tx.QueryRow(ctx, `SELECT contest_id,contest_problem_id,status,mode FROM grader_runs WHERE id=$1 FOR UPDATE`, run).Scan(&contest, &cp, &status, &mode); err != nil {
			return err
		}
		if status != "completed" {
			return ErrInvalid
		}
		if err := tx.QueryRow(ctx, `SELECT id FROM score_publications WHERE grader_run_id=$1`, run).Scan(&publication); err == nil {
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		publication = uuid.NewString()
		if _, err := tx.Exec(ctx, `INSERT INTO score_publications(id,contest_id,grader_run_id,created_by) VALUES($1,$2,$3,NULLIF($4,'')::uuid)`, publication, contest, run, actor); err != nil {
			return err
		}
		var current *string
		if err := tx.QueryRow(ctx, `SELECT current_score_publication_id FROM contests WHERE id=$1 FOR UPDATE`, contest).Scan(&current); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO score_publication_values(publication_id,participant_id,contest_problem_id,score) SELECT $1::uuid,participant_id,contest_problem_id,score FROM score_publication_values WHERE publication_id=$3 AND contest_problem_id<>$4 UNION ALL SELECT $1::uuid,participant_id,contest_problem_id,score FROM grader_scores WHERE grader_run_id=$2`, publication, run, current, cp); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE contests SET current_score_publication_id=$1 WHERE id=$2`, publication, contest); err != nil {
			return err
		}
		return db.Audit(ctx, tx, actor, "grader.publish", run)
	})
	return publication, err
}
func (m Manager) CheckSnapshotAllowed(ctx context.Context, run, participant, submission, attempt string) error {
	var allowed bool
	err := m.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT FROM grader_participants p WHERE p.grader_run_id=$1 AND p.participant_id=$2) AND EXISTS(SELECT FROM grader_inputs i WHERE i.grader_run_id=$1 AND i.participant_id=$2 AND i.submission_id=$3 AND i.judging_attempt_id IS NOT DISTINCT FROM NULLIF($4,'')::uuid)`, run, participant, submission, attempt).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return fmt.Errorf("%w: identifier outside snapshot", ErrInvalid)
	}
	return nil
}
