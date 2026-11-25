package judging

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"podtest/internal/db"
)

const Subject = "podtest.judging.jobs.v1"

func RunWorker(ctx context.Context, pool *pgxpool.Pool, url string) error {
	if os.Getenv("JUDGING_NO_INVOKER") == "true" {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			if err := finishWithoutInvoker(ctx, pool); err != nil && ctx.Err() == nil {
				slog.Error("no-invoker judging", "error", err)
			}
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
			}
		}
	}
	if url == "" {
		return errors.New("NATS_URL is required")
	}
	nc, err := nats.Connect(url, nats.Timeout(5*time.Second), nats.MaxReconnects(-1))
	if err != nil {
		return err
	}
	defer nc.Close()
	js, err := nc.JetStream()
	if err != nil {
		return err
	}
	if _, err = js.StreamInfo("PODTEST_JUDGING"); errors.Is(err, nats.ErrStreamNotFound) {
		_, err = js.AddStream(&nats.StreamConfig{Name: "PODTEST_JUDGING", Subjects: []string{Subject}, Retention: nats.WorkQueuePolicy, Storage: nats.FileStorage, Duplicates: 10 * time.Minute, Discard: nats.DiscardNew})
	}
	if err != nil {
		return err
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := recoverExpired(ctx, pool); err != nil && ctx.Err() == nil {
			slog.Error("expired judging lease recovery", "error", err)
		}
		if err := dispatch(ctx, pool, js); err != nil && ctx.Err() == nil {
			slog.Error("judging outbox", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// recoverExpired retires the queue reservation without fabricating a verdict.
// Keep the lease identity so exact replays of committed events can still ACK.
func recoverExpired(ctx context.Context, pool *pgxpool.Pool) error {
	return db.Tx(ctx, pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM judging_attempts
			WHERE status IN ('assigned','running') AND lease_until IS NOT NULL AND lease_until<=clock_timestamp()
			ORDER BY lease_until,id LIMIT 32 FOR UPDATE SKIP LOCKED`)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := tx.Exec(ctx, `UPDATE judging_attempts SET status='recovery' WHERE id=$1`, id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE queue_entries SET status='done' WHERE attempt_id=$1 AND status='assigned'`, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func finishWithoutInvoker(ctx context.Context, pool *pgxpool.Pool) error {
	return db.Tx(ctx, pool, func(tx pgx.Tx) error {
		var id string
		query := `SELECT j.id FROM judging_attempts j JOIN queue_entries q ON q.attempt_id=j.id WHERE j.status IN ('waiting','queued') AND q.status IN ('waiting','queued') ORDER BY j.created_at,j.id LIMIT 1 FOR UPDATE OF j,q SKIP LOCKED`
		err := tx.QueryRow(ctx, query).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE judging_attempts SET status='error',verdict='testing_error',finished_at=now(),last_sequence=last_sequence+1 WHERE id=$1`, id); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE queue_entries SET status='done' WHERE attempt_id=$1`, id); err != nil {
			return err
		}
		const message = "No execution Invoker is configured"
		_, err = tx.Exec(ctx, `INSERT INTO judging_events(event_id,attempt_id,run_id,sequence,schema_version,kind,payload,occurred_at) SELECT gen_random_uuid(),id,run_id,last_sequence,1,'attempt_finished',jsonb_build_object('status','error','verdict','testing_error','message',$2::text),now() FROM judging_attempts WHERE id=$1`, id, message)
		return err
	})
}
func dispatch(ctx context.Context, pool *pgxpool.Pool, js nats.JetStreamContext) error {
	return db.Tx(ctx, pool, func(tx pgx.Tx) error {
		var contest, attempt string
		err := tx.QueryRow(ctx, `SELECT id FROM contests c WHERE EXISTS(SELECT FROM queue_entries q JOIN contest_settings s ON s.contest_id=q.contest_id WHERE q.contest_id=c.id AND q.status='waiting' AND (s.max_queued_submissions IS NULL OR (SELECT count(*) FROM queue_entries a WHERE a.contest_id=q.contest_id AND a.participant_id=q.participant_id AND a.status IN ('queued','assigned'))<s.max_queued_submissions)) ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&contest)
		if err == nil {
			err = tx.QueryRow(ctx, `SELECT q.attempt_id FROM queue_entries q JOIN contest_settings s ON s.contest_id=q.contest_id WHERE q.contest_id=$1 AND q.status='waiting' AND (s.max_queued_submissions IS NULL OR (SELECT count(*) FROM queue_entries a WHERE a.contest_id=q.contest_id AND a.participant_id=q.participant_id AND a.status IN ('queued','assigned'))<s.max_queued_submissions) ORDER BY q.created_at,q.attempt_id LIMIT 1 FOR UPDATE OF q`, contest).Scan(&attempt)
			if err == nil {
				if _, err = tx.Exec(ctx, `UPDATE judging_attempts SET status='queued' WHERE id=$1 AND status='waiting'`, attempt); err != nil {
					return err
				}
				if _, err = tx.Exec(ctx, `UPDATE queue_entries SET status='queued' WHERE attempt_id=$1`, attempt); err != nil {
					return err
				}
			}
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var id, topic, dedup string
		var data []byte
		err = tx.QueryRow(ctx, `SELECT e.id,e.topic,e.dedup_key,e.payload FROM outbox_events e JOIN queue_entries q ON q.attempt_id::text=e.dedup_key JOIN judging_attempts j ON j.id=q.attempt_id JOIN problem_revisions r ON r.id=j.target_problem_revision_id WHERE e.published_at IS NULL AND q.status='queued' AND j.status='queued' AND r.manifest->'execution_ready'='true'::jsonb ORDER BY e.created_at,e.id LIMIT 1 FOR UPDATE OF e SKIP LOCKED`).Scan(&id, &topic, &dedup, &data)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		publish, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		if _, err = js.Publish(topic, data, nats.MsgId(dedup), nats.Context(publish)); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE outbox_events SET published_at=now() WHERE id=$1`, id)
		return err
	})
}
