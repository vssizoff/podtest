package judging

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	pb "podtest/internal/invokerpb"
)

func reserveQueuedStandard(ctx context.Context, tx pgx.Tx, req *pb.RunLease) error {
	if err := authenticate(ctx, req.GetInvokerId()); err != nil {
		return err
	}
	if !canonicalUUID(req.GetAttemptId()) || !canonicalUUID(req.GetRunId()) || !canonicalUUID(req.GetProcessEpoch()) {
		return status.Error(codes.InvalidArgument, "invalid run identity")
	}
	var state string
	err := tx.QueryRow(ctx, `SELECT status FROM judging_attempts WHERE id=$1 AND run_id=$2 FOR UPDATE`, req.AttemptId, req.RunId).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return status.Error(codes.FailedPrecondition, "inactive run lease")
	}
	if err != nil || state != "queued" {
		return err // An existing assignment is checked by lockLease.
	}
	var queueState string
	err = tx.QueryRow(ctx, `SELECT status FROM queue_entries WHERE attempt_id=$1 FOR UPDATE`, req.AttemptId).Scan(&queueState)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && queueState != "queued") {
		return status.Error(codes.FailedPrecondition, "queued attempt unavailable")
	}
	if err != nil {
		return err
	}
	var eligible bool
	err = tx.QueryRow(ctx, `SELECT r.status='published' AND r.execution_format='standard' AND r.format_version=1
		AND coalesce(r.manifest->'execution_ready'='true'::jsonb AND r.manifest->>'package_format'='ustar_v1',false)
		FROM judging_attempts j JOIN problem_revisions r ON r.id=j.target_problem_revision_id WHERE j.id=$1`, req.AttemptId).Scan(&eligible)
	if err != nil {
		return err
	}
	if !eligible {
		return status.Error(codes.FailedPrecondition, "revision not ready for Standard execution")
	}
	// Serialize reservations across concurrent Claims from the same Invoker.
	var capacity, load int
	err = tx.QueryRow(ctx, `SELECT capacity,load FROM invokers WHERE id=$1 AND protocol_version=1
		AND 'standard'=ANY(capabilities) AND last_heartbeat>clock_timestamp()-interval '30 seconds'
		FOR UPDATE`, req.InvokerId).Scan(&capacity, &load)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && load >= capacity) {
		return status.Error(codes.ResourceExhausted, "no advertised Standard capacity")
	}
	if err != nil {
		return err
	}
	var reserved int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM judging_attempts WHERE invoker_id=$1
		AND status IN ('assigned','running') AND lease_until>clock_timestamp()`, req.InvokerId).Scan(&reserved); err != nil {
		return err
	}
	if reserved >= capacity {
		return status.Error(codes.ResourceExhausted, "no free lease slot")
	}
	if _, err := tx.Exec(ctx, `UPDATE judging_attempts SET status='assigned',invoker_id=$2,
		lease_until=clock_timestamp()+interval '2 minutes' WHERE id=$1`, req.AttemptId, req.InvokerId); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE queue_entries SET status='assigned' WHERE attempt_id=$1`, req.AttemptId)
	return err
}
