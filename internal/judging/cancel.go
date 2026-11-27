package judging

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"podtest/internal/db"
)

var ErrCancelUnavailable = errors.New("attempt cannot be cancelled in its current state")

// Cancel returns pending while the Invoker still owns the reservation.
func (s Submissions) Cancel(ctx context.Context, actor, submission, attempt string) (pending bool, err error) {
	err = db.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		var contest string
		if err := tx.QueryRow(ctx, `SELECT c.id FROM submissions s JOIN contests c ON c.id=s.contest_id
			WHERE s.id=$1 FOR UPDATE OF c`, submission).Scan(&contest); err != nil {
			return err
		}
		var state string
		if err := tx.QueryRow(ctx, `SELECT status FROM judging_attempts WHERE id=$1 AND submission_id=$2 FOR UPDATE`, attempt, submission).Scan(&state); err != nil {
			return err
		}
		switch state {
		case "cancelled":
			return nil
		case "queued", "waiting":
		case "assigned", "running":
			pending = true
			tag, err := tx.Exec(ctx, `UPDATE judging_attempts SET cancel_requested_at=clock_timestamp()
				WHERE id=$1 AND cancel_requested_at IS NULL`, attempt)
			if err != nil || tag.RowsAffected() == 0 {
				return err
			}
			return db.Audit(ctx, tx, actor, "attempt.cancel", attempt)
		default:
			return ErrCancelUnavailable
		}
		tag, err := tx.Exec(ctx, `UPDATE queue_entries SET status='done' WHERE attempt_id=$1 AND status IN ('queued','waiting')`, attempt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrCancelUnavailable
		}
		if _, err := tx.Exec(ctx, `UPDATE judging_attempts SET status='cancelled',finished_at=clock_timestamp(),last_sequence=last_sequence+1 WHERE id=$1`, attempt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO judging_events(event_id,attempt_id,run_id,sequence,schema_version,kind,payload,occurred_at)
			SELECT gen_random_uuid(),id,run_id,last_sequence,1,'attempt_finished','{"status":"cancelled"}',clock_timestamp()
			FROM judging_attempts WHERE id=$1`, attempt); err != nil {
			return err
		}
		return db.Audit(ctx, tx, actor, "attempt.cancel", attempt)
	})
	return pending, err
}
