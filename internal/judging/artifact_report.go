package judging

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"podtest/internal/db"
	pb "podtest/internal/invokerpb"
)

// Report accepts ordered, fenced execution events and finalized artifacts.
func (s *ArtifactControl) Report(ctx context.Context, req *pb.Event) (*pb.Acknowledgement, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "missing event")
	}
	if err := authenticate(ctx, req.GetInvokerId()); err != nil {
		return nil, err
	}
	var terminalState string
	var resultKind string
	var progressKind string
	switch req.Kind {
	case pb.Event_ARTIFACT_CREATED:
		if req.Outcome != "" || !canonicalUUID(req.ArtifactId) {
			return nil, status.Error(codes.InvalidArgument, "invalid artifact event")
		}
	case pb.Event_RUN_FINISHED:
		switch req.Outcome {
		case "infrastructure_error":
			terminalState = "error"
		case "timeout":
			terminalState = "timeout"
		case "success":
			terminalState = "finished"
		default:
			return nil, status.Error(codes.Unimplemented, "unsupported terminal outcome")
		}
		if req.ArtifactId != "" {
			return nil, status.Error(codes.InvalidArgument, "report artifacts separately")
		}
	case pb.Event_RUN_CANCELLED:
		if req.Outcome != "" || req.ArtifactId != "" {
			return nil, status.Error(codes.InvalidArgument, "invalid cancellation fields")
		}
		terminalState = "cancelled"
	case pb.Event_TEST_FINISHED, pb.Event_SUBGROUP_SCORE, pb.Event_PROBLEM_SCORE:
		var err error
		resultKind, err = validateResultReport(req)
		if err != nil {
			return nil, err
		}
	case pb.Event_RUN_STARTED, pb.Event_BUILD_STARTED, pb.Event_BUILD_FINISHED, pb.Event_TEST_STARTED:
		var err error
		progressKind, err = validateProgressReport(req)
		if err != nil {
			return nil, err
		}
	default:
		return nil, status.Error(codes.Unimplemented, "unsupported event kind")
	}
	id, err := uuid.FromBytes(req.EventId)
	if err != nil || id == uuid.Nil || !canonicalUUID(req.AttemptId) || !canonicalUUID(req.RunId) ||
		!canonicalUUID(req.SubmissionId) || len(req.FencingToken) != 32 ||
		req.SchemaVersion != 1 || req.Sequence == 0 || req.Sequence > math.MaxInt64 ||
		req.OccurredAtUnixMillis <= 0 || req.OccurredAtUnixMillis > 253402300799999 ||
		len(req.ProtoReflect().GetUnknown()) != 0 {
		return nil, status.Error(codes.InvalidArgument, "invalid run event")
	}
	if resultKind == "" && progressKind == "" && (req.Score != nil || req.TestId != "" || req.SubgroupId != "" ||
		(terminalState == "finished" && !validReportVerdict(req.Verdict)) || (terminalState != "finished" && req.Verdict != "")) {
		return nil, status.Error(codes.InvalidArgument, "invalid terminal or artifact fields")
	}
	wire, err := (proto.MarshalOptions{Deterministic: true}).Marshal(req)
	if err != nil {
		return nil, publicError(err)
	}
	sum := sha256.Sum256(wire)
	digest := hex.EncodeToString(sum[:])
	var out pb.Acknowledgement
	err = db.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		var active, cancelRequested bool
		var token []byte
		var last int64
		err := tx.QueryRow(ctx, `SELECT fencing_token,last_sequence FROM judging_attempts
			WHERE id=$1 AND run_id=$2 AND submission_id=$3 AND invoker_id=$4 FOR UPDATE`,
			req.AttemptId, req.RunId, req.SubmissionId, req.InvokerId).Scan(&token, &last)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && subtle.ConstantTimeCompare(token, req.FencingToken) != 1) {
			return status.Error(codes.FailedPrecondition, "run lease fenced")
		}
		if err != nil {
			return err
		}
		// A lost ACK can be recovered after expiry, but only for the exact
		// committed event and while the request still matches the stored fence.
		var same bool
		err = tx.QueryRow(ctx, `SELECT coalesce(payload->>'report_digest'=$2,false) FROM judging_events
			WHERE event_id=$1 AND attempt_id=$3 AND run_id=$4`, id, digest, req.AttemptId, req.RunId).Scan(&same)
		if err == nil {
			if !same {
				return status.Error(codes.AlreadyExists, "event identity already bound")
			}
			out.Duplicate = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT coalesce(status IN ('assigned','running') AND lease_until>clock_timestamp(),false),
			cancel_requested_at IS NOT NULL FROM judging_attempts WHERE id=$1`, req.AttemptId).Scan(&active, &cancelRequested); err != nil {
			return err
		}
		if !active || int64(req.Sequence)-1 != last {
			return status.Error(codes.FailedPrecondition, "inactive lease or unexpected event sequence")
		}
		if terminalState != "" {
			if terminalState == "cancelled" && !cancelRequested {
				return status.Error(codes.FailedPrecondition, "no cancellation requested")
			}
			return finishReportedRun(ctx, tx, req, id, digest, terminalState)
		}
		if resultKind != "" {
			return recordResultReport(ctx, tx, req, id, digest, resultKind)
		}
		if progressKind != "" {
			return recordProgressReport(ctx, tx, req, id, digest, progressKind)
		}
		tag, err := tx.Exec(ctx, `INSERT INTO judging_events(event_id,attempt_id,run_id,sequence,schema_version,kind,payload,occurred_at)
			SELECT $1,g.attempt_id,g.run_id,$4,1,'artifact_created',jsonb_build_object('artifact_id',g.id,'kind',g.kind,'report_digest',$5::text),$6
			FROM invoker_upload_grants g JOIN attempt_artifacts a ON a.attempt_id=g.attempt_id AND a.artifact_id=g.id AND a.kind=g.kind
			WHERE g.attempt_id=$2 AND g.run_id=$3 AND g.id=$7 AND g.completed_at IS NOT NULL
			AND NOT EXISTS (SELECT FROM judging_events e WHERE e.attempt_id=g.attempt_id AND e.run_id=g.run_id
				AND e.kind='artifact_created' AND e.payload->>'artifact_id'=g.id::text)
			ON CONFLICT DO NOTHING`, id, req.AttemptId, req.RunId, int64(req.Sequence), digest, time.UnixMilli(req.OccurredAtUnixMillis), req.ArtifactId)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return status.Error(codes.FailedPrecondition, "artifact or event identity unavailable")
		}
		_, err = tx.Exec(ctx, `UPDATE judging_attempts SET last_sequence=$2 WHERE id=$1`, req.AttemptId, int64(req.Sequence))
		return err
	})
	if err != nil {
		return nil, publicError(err)
	}
	return &out, nil
}

func finishReportedRun(ctx context.Context, tx pgx.Tx, req *pb.Event, id uuid.UUID, digest, state string) error {
	if state == "finished" {
		if err := publishReportedResults(ctx, tx, req); err != nil {
			return err
		}
	}
	tag, err := tx.Exec(ctx, `INSERT INTO judging_events(event_id,attempt_id,run_id,sequence,schema_version,kind,payload,occurred_at)
		VALUES($1,$2,$3,$4,1,'attempt_finished',jsonb_build_object('status',$5::text,'outcome',$6::text,'report_digest',$7::text),$8)
		ON CONFLICT DO NOTHING`, id, req.AttemptId, req.RunId, int64(req.Sequence), state, req.Outcome, digest, time.UnixMilli(req.OccurredAtUnixMillis))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return status.Error(codes.AlreadyExists, "event identity already bound")
	}
	tag, err = tx.Exec(ctx, `UPDATE judging_attempts SET status=$2,finished_at=clock_timestamp(),last_sequence=$3,
		verdict=CASE WHEN $2='error' THEN 'testing_error' ELSE verdict END
		WHERE id=$1 AND lease_until>clock_timestamp()`, req.AttemptId, state, int64(req.Sequence))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return status.Error(codes.FailedPrecondition, "run lease expired")
	}
	_, err = tx.Exec(ctx, `UPDATE queue_entries SET status='done' WHERE attempt_id=$1 AND status='assigned'`, req.AttemptId)
	if err == nil && state == "finished" {
		_, err = tx.Exec(ctx, `INSERT INTO grader_triggers(attempt_id,contest_problem_id)
			SELECT j.id,cp.id FROM judging_attempts j JOIN submissions s ON s.id=j.submission_id
			JOIN contest_problems cp ON cp.id=s.contest_problem_id JOIN problem_revisions r ON r.id=cp.problem_revision_id
			WHERE j.id=$1 AND cp.detached_at IS NULL AND r.grader_mode='automatic_submission'
			ON CONFLICT DO NOTHING`, req.AttemptId)
	}
	return err
}
