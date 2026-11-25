package judging

import (
	"context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	pb "podtest/internal/invokerpb"
)

// Heartbeat registers liveness and capacity only. It never grants execution
// authority and therefore cannot make an execution-unready Invoker schedulable.
func (s *ArtifactControl) Heartbeat(ctx context.Context, req *pb.HeartbeatRequest) (*pb.Acknowledgement, error) {
	if err := authenticate(ctx, req.GetInvokerId()); err != nil {
		return nil, err
	}
	if req.ProtocolVersion != 1 || req.Capacity > 4096 || req.Load > req.Capacity || len(req.ExecutionFormats) > 2 || (req.Capacity > 0 && len(req.ExecutionFormats) == 0) {
		return nil, status.Error(codes.InvalidArgument, "invalid heartbeat")
	}
	for _, format := range req.ExecutionFormats {
		if format != "standard" && format != "scripted_lua" {
			return nil, status.Error(codes.InvalidArgument, "unsupported execution format")
		}
	}
	if req.Capacity == 0 {
		_, err := s.Pool.Exec(ctx, `UPDATE invokers SET capacity=0,load=0,last_heartbeat=clock_timestamp() WHERE id=$1`, req.InvokerId)
		if err != nil {
			return nil, publicError(err)
		}
		return &pb.Acknowledgement{}, nil
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO invokers(id,protocol_version,capabilities,capacity,load)
		VALUES($1,$2,coalesce($3::text[],ARRAY[]::text[]),$4,$5)
		ON CONFLICT (id) DO UPDATE SET protocol_version=excluded.protocol_version,
		capabilities=excluded.capabilities,capacity=excluded.capacity,load=excluded.load,last_heartbeat=clock_timestamp()`,
		req.InvokerId, req.ProtocolVersion, req.ExecutionFormats, req.Capacity, req.Load)
	if err != nil {
		return nil, publicError(err)
	}
	return &pb.Acknowledgement{}, nil
}
