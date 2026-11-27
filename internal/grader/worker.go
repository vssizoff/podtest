package grader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os/exec"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"podtest/internal/db"
)

type SnapshotScore struct {
	Input
	Score    *string `json:"base_score"`
	Verdict  *string `json:"verdict"`
	Language string  `json:"language"`
}

func (m Manager) SnapshotScores(ctx context.Context, run, token string) ([]SnapshotScore, error) {
	if _, _, err := m.Snapshot(ctx, run, token); err != nil {
		return nil, err
	}
	return m.snapshotScores(ctx, run)
}

func (m Manager) snapshotScores(ctx context.Context, run string) ([]SnapshotScore, error) {
	rows, err := m.Pool.Query(ctx, `SELECT i.submission_id,i.judging_attempt_id,i.participant_id,j.base_score::text,j.verdict,l.code
		FROM grader_inputs i LEFT JOIN judging_attempts j ON j.id=i.judging_attempt_id AND j.submission_id=i.submission_id
		JOIN submissions s ON s.id=i.submission_id JOIN languages l ON l.id=s.language_id
		WHERE i.grader_run_id=$1 ORDER BY i.submission_id`, run)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (item SnapshotScore, err error) {
		err = row.Scan(&item.SubmissionID, &item.AttemptID, &item.ParticipantID, &item.Score, &item.Verdict, &item.Language)
		return
	})
}

func (m Manager) claim(ctx context.Context) (run Run, err error) {
	err = db.Tx(ctx, m.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE grader_runs SET status='failed',finished_at=now(),log='Worker lease expired'
			WHERE status='running' AND capability_expires_at<=now()`); err != nil {
			return err
		}
		var attempt, contest, problem string
		e := tx.QueryRow(ctx, `SELECT t.attempt_id,cp.contest_id,cp.id FROM grader_triggers t
			JOIN contest_problems cp ON cp.id=t.contest_problem_id ORDER BY t.created_at,t.attempt_id LIMIT 1 FOR UPDATE OF t SKIP LOCKED`).Scan(&attempt, &contest, &problem)
		if e == nil {
			if _, e = tx.Exec(ctx, `DELETE FROM grader_triggers WHERE attempt_id=$1`, attempt); e != nil {
				return e
			}
			_, _, e = m.createTx(ctx, tx, "", contest, problem, "automatic_submission", "preview")
			if errors.Is(e, ErrInvalid) || errors.Is(e, pgx.ErrNoRows) {
				e = nil
			}
			if e != nil {
				return e
			}
		} else if !errors.Is(e, pgx.ErrNoRows) {
			return e
		}
		e = tx.QueryRow(ctx, `WITH next AS (
			SELECT id FROM grader_runs WHERE status='pending' ORDER BY created_at,id LIMIT 1 FOR UPDATE SKIP LOCKED)
			UPDATE grader_runs g SET status='running',capability_expires_at=clock_timestamp()+interval '2 minutes',
			capability_hash=sha256(convert_to(secret.token,'UTF8')) FROM next CROSS JOIN
			(SELECT replace(gen_random_uuid()::text || gen_random_uuid()::text,'-','') AS token) secret
			WHERE g.id=next.id RETURNING g.id,g.contest_id,g.contest_problem_id,g.snapshot_id,g.snapshot_digest,g.mode,g.status,secret.token,g.capability_expires_at`).Scan(
			&run.ID, &run.ContestID, &run.ProblemID, &run.SnapshotID, &run.SnapshotDigest, &run.Mode, &run.Status, &run.Capability, &run.Expires)
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		return e
	})
	return
}

var imageID = regexp.MustCompile(`^(sha256:)?[a-f0-9]{64}$`)

func (m Manager) Work(ctx context.Context, image string) error {
	if !imageID.MatchString(image) {
		return errors.New("GRADER_IMAGE_ID must identify a provisioned Python image by digest")
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		run, err := m.claim(ctx)
		if err == nil && run.ID != "" {
			if err = m.execute(ctx, image, run); err != nil {
				slog.Error("grader execution failed", "run", run.ID, "error", err)
				if _, markErr := m.Pool.Exec(ctx, `UPDATE grader_runs SET status='failed',finished_at=now(),log='Grader execution failed' WHERE id=$1 AND status='running'`, run.ID); markErr != nil && ctx.Err() == nil {
					slog.Error("grader failure status update failed", "error", markErr)
				}
			}
		} else if err != nil && ctx.Err() == nil {
			slog.Error("grader claim failed", "error", err)
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
	return nil
}

type boundedBuffer struct{ bytes.Buffer }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 4<<20 {
		return 0, errors.New("grader output too large")
	}
	return b.Buffer.Write(p)
}

func (m Manager) execute(ctx context.Context, image string, run Run) error {
	_, scope, err := m.Snapshot(ctx, run.ID, run.Capability)
	if err != nil {
		return err
	}
	inputs, err := m.snapshotScores(ctx, run.ID)
	if err != nil {
		return err
	}
	var scriptArtifact *string
	err = m.Pool.QueryRow(ctx, `SELECT r.grader_script_artifact_id FROM grader_runs g JOIN problem_revisions r ON r.id=g.problem_revision_id WHERE g.id=$1`, run.ID).Scan(&scriptArtifact)
	if err != nil {
		return err
	}
	var script []byte
	if scriptArtifact != nil {
		if m.Artifacts == nil {
			return errors.New("grader script storage is not configured")
		}
		script, err = m.Artifacts.Read(ctx, *scriptArtifact, 256<<10)
		if err != nil {
			return err
		}
	}
	data, err := json.Marshal(struct {
		Run          Run             `json:"run"`
		Participants []string        `json:"participants"`
		Inputs       []SnapshotScore `json:"inputs"`
		Script       string          `json:"script"`
	}{run, scope.Participants, inputs, string(script)})
	if err != nil {
		return err
	}
	deadline, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	name := "podtest-grader-" + run.ID
	cmd := exec.CommandContext(deadline, "podman", "run", "--rm", "--name", name, "--pull=never",
		"--network=none", "--read-only", "--cap-drop=all", "--security-opt=no-new-privileges", "--user=65534:65534",
		"--memory=128m", "--memory-swap=128m", "--cpus=1", "--pids-limit=16", "--http-proxy=false", "--image-volume=ignore",
		"-i", "--entrypoint=python3", image, "-I", "-B", "-c", builtinGrader)
	cmd.Stdin = bytes.NewReader(data)
	var output boundedBuffer
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer stop()
		_ = exec.CommandContext(cleanup, "podman", "rm", "--force", "--ignore", name).Run()
		return err
	}
	var result ScorePayload
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		return err
	}
	if err := m.Submit(ctx, run.ID, run.Capability, result); err != nil {
		return err
	}
	if run.Mode == "publish" {
		_, err = m.Publish(ctx, "", run.ID)
	}
	return err
}

const builtinGrader = `
import json, sys
from decimal import Decimal

snapshot = json.load(sys.stdin)
script = snapshot.pop('script')
if script:
    namespace = {'__name__': '__grader__'}
    exec(compile(script, 'grader.py', 'exec'), namespace)
    json.dump(namespace['grade'](snapshot), sys.stdout, allow_nan=False)
    sys.exit(0)
scores = {p: Decimal(0) for p in snapshot['participants'] or []}
for item in snapshot['inputs'] or []:
    participant = item['participant_id']
    score = item['base_score']
    if score is not None and participant in scores:
        scores[participant] = max(scores[participant], Decimal(score))
problem = snapshot['run']['contest_problem_id']
json.dump({'scores': [{'participant_id': p, 'contest_problem_id': problem, 'score': format(s, 'f')}
                      for p, s in scores.items()], 'log': 'Computed maximum base score per participant.'}, sys.stdout)
`
