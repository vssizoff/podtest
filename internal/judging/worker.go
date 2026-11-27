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
	"podtest/internal/problem"
)

const (
	Subject     = "podtest.judging.jobs.v1"
	luaSubject  = "podtest.judging.lua.v1"
	jobStream   = "PODTEST_JUDGING"
	jobConsumer = "podtest-standard"
)

func RunWorker(ctx context.Context, pool *pgxpool.Pool, url string) error {
	if os.Getenv("JUDGING_NO_INVOKER") == "true" {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			if err := failUnassigned(ctx, pool, false, "No execution Invoker is configured"); err != nil && ctx.Err() == nil {
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
	if err = provisionStandardQueue(ctx, js); err != nil {
		return err
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := recoverExpired(ctx, pool); err != nil && ctx.Err() == nil {
			slog.Error("expired judging lease recovery", "error", err)
		}
		if err := retryExpiredStandard(ctx, pool); err != nil && ctx.Err() == nil {
			slog.Error("expired Standard attempt retry", "error", err)
		}
		if err := failUnassigned(ctx, pool, true, "Task revision is not available for execution"); err != nil && ctx.Err() == nil {
			slog.Error("unavailable judging revision", "error", err)
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

func provisionStandardQueue(ctx context.Context, js nats.JetStreamContext) error {
	stream, err := js.StreamInfo(jobStream, nats.Context(ctx))
	if errors.Is(err, nats.ErrStreamNotFound) {
		stream, err = js.AddStream(&nats.StreamConfig{Name: jobStream, Subjects: []string{Subject, luaSubject}, Retention: nats.WorkQueuePolicy, Storage: nats.FileStorage, Duplicates: 10 * time.Minute, Discard: nats.DiscardNew}, nats.Context(ctx))
	}
	if err != nil {
		return err
	}
	if stream.Config.Retention != nats.WorkQueuePolicy || stream.Config.Storage != nats.FileStorage {
		return errors.New("incompatible judging stream configuration")
	}
	subjects := stream.Config.Subjects
	if len(subjects) == 1 && subjects[0] == Subject {
		stream.Config.Subjects = []string{Subject, luaSubject}
		if _, err = js.UpdateStream(&stream.Config, nats.Context(ctx)); err != nil {
			return err
		}
	} else if len(subjects) != 2 || !((subjects[0] == Subject && subjects[1] == luaSubject) || (subjects[1] == Subject && subjects[0] == luaSubject)) {
		return errors.New("incompatible judging stream subjects")
	}
	if err = provisionConsumer(ctx, js, jobConsumer, Subject); err != nil {
		return err
	}
	return provisionConsumer(ctx, js, "podtest-scripted-lua", luaSubject)
}

func provisionConsumer(ctx context.Context, js nats.JetStreamContext, name, subject string) error {
	consumer, err := js.ConsumerInfo(jobStream, name, nats.Context(ctx))
	if errors.Is(err, nats.ErrConsumerNotFound) {
		consumer, err = js.AddConsumer(jobStream, &nats.ConsumerConfig{Durable: name, FilterSubject: subject, AckPolicy: nats.AckExplicitPolicy, AckWait: 30 * time.Second}, nats.Context(ctx))
	}
	if err != nil {
		return err
	}
	cfg := consumer.Config
	if cfg.Durable != name || cfg.AckPolicy != nats.AckExplicitPolicy || cfg.DeliverSubject != "" || cfg.HeadersOnly || cfg.FilterSubject != subject || len(cfg.FilterSubjects) != 0 {
		return errors.New("incompatible judging consumer configuration")
	}
	return nil
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
			if _, err := tx.Exec(ctx, `WITH expired AS (
				UPDATE judging_attempts SET
					status=CASE WHEN cancel_requested_at IS NULL THEN 'recovery' ELSE 'cancelled' END,
					finished_at=CASE WHEN cancel_requested_at IS NOT NULL THEN clock_timestamp() ELSE finished_at END,
					last_sequence=last_sequence+CASE WHEN cancel_requested_at IS NOT NULL THEN 1 ELSE 0 END
				WHERE id=$1 RETURNING id,run_id,last_sequence,status)
				INSERT INTO judging_events(event_id,attempt_id,run_id,sequence,schema_version,kind,payload,occurred_at)
				SELECT gen_random_uuid(),id,run_id,last_sequence,1,'attempt_finished',
					'{"status":"cancelled","outcome":"lease_expired"}',clock_timestamp()
				FROM expired WHERE status='cancelled'`, id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE queue_entries SET status='done' WHERE attempt_id=$1 AND status='assigned'`, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// Retry a lost Standard lease once with a new attempt and run identity. The
// retired attempt stays fenced and still acknowledges exact committed events.
func retryExpiredStandard(ctx context.Context, pool *pgxpool.Pool) error {
	return db.Tx(ctx, pool, func(tx pgx.Tx) error {
		var contest, id, participant, revision, score string
		var cap *int
		err := tx.QueryRow(ctx, `SELECT c.id,j.submission_id,s.participant_id,j.target_problem_revision_id,j.max_score::text,cs.max_queued_submissions
			FROM judging_attempts j JOIN submissions s ON s.id=j.submission_id
			JOIN contests c ON c.id=s.contest_id JOIN contest_settings cs ON cs.contest_id=c.id
			JOIN problem_revisions r ON r.id=j.target_problem_revision_id
			WHERE j.status='recovery' AND j.trigger_reason<>'lease_recovery' AND c.archived_at IS NULL
			AND `+problem.RunnableRevisionSQL+`
			AND NOT EXISTS (SELECT FROM judging_attempts newer WHERE newer.submission_id=s.id AND newer.attempt_number>j.attempt_number)
			ORDER BY j.lease_until,j.id LIMIT 1 FOR UPDATE OF c SKIP LOCKED`).Scan(&contest, &id, &participant, &revision, &score, &cap)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		// READ COMMITTED can select from a snapshot taken before a concurrent
		// rejudge committed. Recheck the latest attempt under the contest lock.
		var eligible bool
		if err := tx.QueryRow(ctx, `SELECT status='recovery' AND trigger_reason<>'lease_recovery'
			FROM judging_attempts WHERE submission_id=$1 ORDER BY attempt_number DESC LIMIT 1`, id).Scan(&eligible); err != nil || !eligible {
			return err
		}
		_, err = createAttempt(ctx, tx, id, contest, participant, revision, score, nil, "lease_recovery", cap)
		return err
	})
}

func failUnassigned(ctx context.Context, pool *pgxpool.Pool, onlyUnavailable bool, message string) error {
	return db.Tx(ctx, pool, func(tx pgx.Tx) error {
		var id string
		query := `SELECT j.id FROM judging_attempts j JOIN queue_entries q ON q.attempt_id=j.id
			JOIN problem_revisions r ON r.id=j.target_problem_revision_id
			WHERE j.status IN ('waiting','queued') AND q.status IN ('waiting','queued')
			AND (NOT $1::boolean OR NOT ` + problem.RunnableRevisionSQL + `)
			ORDER BY j.created_at,j.id LIMIT 1 FOR UPDATE OF j,q SKIP LOCKED`
		err := tx.QueryRow(ctx, query, onlyUnavailable).Scan(&id)
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
		err = tx.QueryRow(ctx, `SELECT e.id,e.topic,e.dedup_key,e.payload FROM outbox_events e JOIN queue_entries q ON q.attempt_id::text=e.dedup_key JOIN judging_attempts j ON j.id=q.attempt_id JOIN problem_revisions r ON r.id=j.target_problem_revision_id
			WHERE e.published_at IS NULL AND e.topic IN ($1,$2) AND q.status='queued' AND j.status='queued' AND `+problem.RunnableRevisionSQL+`
			ORDER BY e.created_at,e.id LIMIT 1 FOR UPDATE OF e SKIP LOCKED`, Subject, luaSubject).Scan(&id, &topic, &dedup, &data)
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
