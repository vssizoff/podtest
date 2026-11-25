package judging

import (
	"context"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	pb "podtest/internal/invokerpb"
)

var reportID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
var reportScore = regexp.MustCompile(`^(0|[1-9][0-9]{0,11})(\.[0-9]{1,6})?$`)

func validReportVerdict(s string) bool { return reportID.MatchString(s) }

func validateResultReport(req *pb.Event) (string, error) {
	invalid := status.Error(codes.InvalidArgument, "invalid result fields")
	if req.Outcome != "" || req.ArtifactId != "" || req.Score == nil || !reportScore.MatchString(req.GetScore()) {
		return "", invalid
	}
	switch req.Kind {
	case pb.Event_TEST_FINISHED:
		if reportID.MatchString(req.TestId) && req.SubgroupId == "" && validReportVerdict(req.Verdict) {
			return "test_finished", nil
		}
	case pb.Event_SUBGROUP_SCORE:
		if req.TestId == "" && reportID.MatchString(req.SubgroupId) && req.Verdict == "" {
			return "subgroup_score", nil
		}
	case pb.Event_PROBLEM_SCORE:
		if req.TestId == "" && req.SubgroupId == "" && req.Verdict == "" {
			return "problem_score", nil
		}
	}
	return "", invalid
}

func recordResultReport(ctx context.Context, tx pgx.Tx, req *pb.Event, id uuid.UUID, digest, kind string) error {
	key := req.TestId + req.SubgroupId
	tag, err := tx.Exec(ctx, `INSERT INTO judging_events(event_id,attempt_id,run_id,sequence,schema_version,kind,payload,occurred_at)
		SELECT $1,j.id,j.run_id,$4,1,$5,jsonb_build_object('key',$6::text,'verdict',$7::text,'score',$8::text,'report_digest',$9::text),$10
		FROM judging_attempts j WHERE j.id=$2 AND j.run_id=$3 AND j.lease_until>clock_timestamp()
		AND $8::text::numeric <= j.max_score
		AND NOT EXISTS (SELECT FROM judging_events e WHERE e.attempt_id=j.id AND e.run_id=j.run_id AND e.kind=$5 AND e.payload->>'key'=$6)
		ON CONFLICT DO NOTHING`, id, req.AttemptId, req.RunId, int64(req.Sequence), kind, key, req.Verdict, req.GetScore(), digest, time.UnixMilli(req.OccurredAtUnixMillis))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return status.Error(codes.FailedPrecondition, "expired lease, duplicate result, or score exceeds maximum")
	}
	_, err = tx.Exec(ctx, `UPDATE judging_attempts SET last_sequence=$2 WHERE id=$1`, req.AttemptId, int64(req.Sequence))
	return err
}

func publishReportedResults(ctx context.Context, tx pgx.Tx, req *pb.Event) error {
	tag, err := tx.Exec(ctx, `UPDATE judging_attempts j SET verdict=$3,base_score=(e.payload->>'score')::numeric
		FROM judging_events e WHERE j.id=$1 AND j.run_id=$2 AND e.attempt_id=j.id AND e.run_id=j.run_id AND e.kind='problem_score'`, req.AttemptId, req.RunId, req.Verdict)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return status.Error(codes.FailedPrecondition, "successful run requires a problem score")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO test_results(attempt_id,test_id,verdict,score)
		SELECT attempt_id,payload->>'key',payload->>'verdict',(payload->>'score')::numeric
		FROM judging_events WHERE attempt_id=$1 AND run_id=$2 AND kind='test_finished'`, req.AttemptId, req.RunId); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO subgroup_results(attempt_id,subgroup_id,score)
		SELECT attempt_id,payload->>'key',(payload->>'score')::numeric
		FROM judging_events WHERE attempt_id=$1 AND run_id=$2 AND kind='subgroup_score'`, req.AttemptId, req.RunId)
	return err
}
