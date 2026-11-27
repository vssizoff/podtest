package judging

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	pb "podtest/internal/invokerpb"
)

func validateProgressReport(req *pb.Event) (string, error) {
	invalid := status.Error(codes.InvalidArgument, "invalid progress fields")
	if req.Outcome != "" || req.ArtifactId != "" || req.SubgroupId != "" || req.Verdict != "" || req.Score != nil {
		return "", invalid
	}
	if req.Kind == pb.Event_TEST_STARTED {
		if !reportID.MatchString(req.TestId) {
			return "", invalid
		}
		return "test_started", nil
	}
	if req.TestId != "" {
		return "", invalid
	}
	switch req.Kind {
	case pb.Event_RUN_STARTED:
		return "run_started", nil
	case pb.Event_BUILD_STARTED:
		return "build_started", nil
	case pb.Event_BUILD_FINISHED:
		return "build_finished", nil
	default:
		return "", invalid
	}
}

func recordProgressReport(ctx context.Context, tx pgx.Tx, req *pb.Event, id uuid.UUID, digest, kind string) error {
	tag, err := tx.Exec(ctx, `INSERT INTO judging_events(event_id,attempt_id,run_id,sequence,schema_version,kind,payload,occurred_at)
		SELECT $1,j.id,j.run_id,$4,1,$5,jsonb_build_object('key',$6::text,'report_digest',$7::text),$8
		FROM judging_attempts j WHERE j.id=$2 AND j.run_id=$3 AND j.lease_until>clock_timestamp()
		AND NOT EXISTS (SELECT FROM judging_events e WHERE e.attempt_id=j.id
			AND (e.kind=$5 OR ($5='test_started' AND e.kind='test_finished')) AND e.payload->>'key'=$6)
		ON CONFLICT DO NOTHING`, id, req.AttemptId, req.RunId, int64(req.Sequence), kind, req.TestId, digest, time.UnixMilli(req.OccurredAtUnixMillis))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return status.Error(codes.FailedPrecondition, "expired lease or repeated progress event")
	}
	_, err = tx.Exec(ctx, `UPDATE judging_attempts SET status='running',started_at=coalesce(started_at,clock_timestamp()),last_sequence=$2 WHERE id=$1`, req.AttemptId, int64(req.Sequence))
	return err
}

func (s Submissions) loadProgress(ctx context.Context, attempt *Attempt) error {
	return s.Pool.QueryRow(ctx, `SELECT
		coalesce((SELECT CASE kind WHEN 'build_started' THEN 'building' WHEN 'build_finished' THEN 'running' ELSE 'testing' END
			FROM judging_events WHERE attempt_id=$1 AND kind IN ('build_started','build_finished','test_started','test_finished')
			ORDER BY sequence DESC LIMIT 1), ''),
		ARRAY(SELECT started.payload->>'key' FROM judging_events started
			WHERE started.attempt_id=$1 AND started.kind='test_started'
			AND (NOT EXISTS (SELECT FROM judging_attempts j JOIN problem_revisions r ON r.id=j.target_problem_revision_id
				WHERE j.id=$1 AND r.execution_format='standard')
				OR started.sequence=(SELECT max(sequence) FROM judging_events WHERE attempt_id=$1 AND kind='test_started'))
			AND NOT EXISTS (SELECT FROM judging_events finished WHERE finished.attempt_id=$1
				AND finished.kind='test_finished' AND finished.payload->>'key'=started.payload->>'key')
			ORDER BY started.sequence LIMIT 64)`, attempt.ID).Scan(&attempt.Phase, &attempt.ActiveTests)
}
