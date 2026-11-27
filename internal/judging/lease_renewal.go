package judging

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"podtest/internal/db"
	pb "podtest/internal/invokerpb"
)

// RenewLease extends an existing reservation, never schedules or resurrects one.
func (s *ArtifactControl) RenewLease(ctx context.Context, req *pb.RunLease) (*pb.RenewLeaseResponse, error) {
	var out pb.RenewLeaseResponse
	err := db.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := lockLease(ctx, tx, req, false); err != nil {
			return err
		}
		// Bound the extension relative to the database clock so retries cannot
		// accumulate lease time and an early renewal never shortens the grant.
		err := tx.QueryRow(ctx, `UPDATE judging_attempts
			SET lease_until=CASE WHEN cancel_requested_at IS NULL
				THEN greatest(lease_until,clock_timestamp()+interval '2 minutes') ELSE lease_until END
			WHERE id=$1 AND lease_until>clock_timestamp()
			RETURNING floor(extract(epoch FROM lease_until))::bigint,cancel_requested_at IS NOT NULL`, req.AttemptId).Scan(&out.LeaseExpiresUnix, &out.CancelRequested)
		if errors.Is(err, pgx.ErrNoRows) {
			return status.Error(codes.FailedPrecondition, "run lease expired")
		}
		return err
	})
	if err != nil {
		return nil, publicError(err)
	}
	return &out, nil
}
