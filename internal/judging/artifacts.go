package judging

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"podtest/internal/artifact"
	"podtest/internal/db"
	pb "podtest/internal/invokerpb"
)

// ArtifactControl grants fenced leases and serves execution inputs and reports.
type ArtifactControl struct {
	pb.UnimplementedControlServer
	Pool  *pgxpool.Pool
	Store *artifact.Store
}

func canonicalUUID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id != uuid.Nil && id.String() == s
}

func authenticate(ctx context.Context, invoker string) error {
	p, ok := peer.FromContext(ctx)
	if ok && canonicalUUID(invoker) {
		tls, ok := p.AuthInfo.(credentials.TLSInfo)
		if ok && len(tls.State.VerifiedChains) > 0 && tls.State.VerifiedChains[0][0].Subject.CommonName == invoker {
			return nil
		}
	}
	return status.Error(codes.Unauthenticated, "verified Invoker certificate required")
}

type storedLease struct {
	expires time.Time
	token   []byte
}

func lockLease(ctx context.Context, tx pgx.Tx, lease *pb.RunLease, claim bool) (storedLease, error) {
	var out storedLease
	if err := authenticate(ctx, lease.GetInvokerId()); err != nil {
		return out, err
	}
	if !canonicalUUID(lease.GetAttemptId()) || !canonicalUUID(lease.GetRunId()) || !canonicalUUID(lease.GetProcessEpoch()) {
		return out, status.Error(codes.InvalidArgument, "invalid run identity")
	}
	var epoch string
	err := tx.QueryRow(ctx, `SELECT lease_until,coalesce(process_epoch::text,''),fencing_token FROM judging_attempts
		WHERE id=$1 AND run_id=$2 AND invoker_id=$3 AND status IN ('assigned','running') AND lease_until>clock_timestamp() FOR UPDATE`,
		lease.AttemptId, lease.RunId, lease.InvokerId).Scan(&out.expires, &epoch, &out.token)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, status.Error(codes.FailedPrecondition, "inactive run lease")
	}
	if err != nil {
		return out, err
	}
	if claim && epoch == "" {
		out.token = make([]byte, 32)
		if _, err := rand.Read(out.token); err != nil {
			return out, err
		}
		_, err = tx.Exec(ctx, `UPDATE judging_attempts SET process_epoch=$2,fencing_token=$3 WHERE id=$1`, lease.AttemptId, lease.ProcessEpoch, out.token)
		return out, err
	}
	if epoch != lease.ProcessEpoch || len(out.token) != 32 || (!claim && subtle.ConstantTimeCompare(out.token, lease.FencingToken) != 1) {
		return out, status.Error(codes.FailedPrecondition, "run lease fenced")
	}
	return out, nil
}

func ttl(expires time.Time) (time.Duration, error) {
	d := min(5*time.Minute, time.Until(expires).Truncate(time.Second)-time.Second)
	if d < time.Second {
		return 0, status.Error(codes.FailedPrecondition, "run lease expiring")
	}
	return d, nil
}

func (s *ArtifactControl) Claim(ctx context.Context, req *pb.ClaimRequest) (*pb.ClaimResponse, error) {
	return s.inputs(ctx, &pb.RunLease{AttemptId: req.GetAttemptId(), RunId: req.GetRunId(), InvokerId: req.GetInvokerId(), ProcessEpoch: req.GetProcessEpoch()}, true)
}

func (s *ArtifactControl) RefreshArtifacts(ctx context.Context, req *pb.RunLease) (*pb.ClaimResponse, error) {
	return s.inputs(ctx, req, false)
}

func (s *ArtifactControl) inputs(ctx context.Context, req *pb.RunLease, claim bool) (*pb.ClaimResponse, error) {
	var out pb.ClaimResponse
	err := db.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		if claim {
			if err := reserveQueuedStandard(ctx, tx, req); err != nil {
				return err
			}
		}
		lease, err := lockLease(ctx, tx, req, claim)
		if err != nil {
			return err
		}
		var sourceKey, bundleKey, sourceDigest, bundleDigest string
		var sourceSize, bundleSize int64
		err = tx.QueryRow(ctx, `SELECT src.storage_key,src.digest,src.size,b.storage_key,b.digest,b.size
			FROM judging_attempts a JOIN submissions s ON s.id=a.submission_id JOIN artifacts src ON src.id=s.source_artifact_id
			JOIN problem_revisions r ON r.id=a.target_problem_revision_id JOIN artifacts b ON b.id=r.bundle_artifact_id
			WHERE a.id=$1 AND s.digest=src.digest AND r.status='published' AND r.manifest->>'package_format'='ustar_v1' AND r.digest=b.digest`, req.AttemptId).
			Scan(&sourceKey, &sourceDigest, &sourceSize, &bundleKey, &bundleDigest, &bundleSize)
		if errors.Is(err, pgx.ErrNoRows) {
			return status.Error(codes.FailedPrecondition, "pinned USTAR bundle unavailable")
		}
		if err != nil {
			return err
		}
		d, err := ttl(lease.expires)
		if err != nil {
			return err
		}
		out.Source, err = descriptor(sourceDigest, sourceSize)
		if err != nil {
			return err
		}
		out.Bundle, err = descriptor(bundleDigest, bundleSize)
		if err != nil {
			return err
		}
		out.SourceUrl, err = s.Store.PresignDownload(ctx, sourceKey, d)
		if err != nil {
			return err
		}
		out.BundleUrl, err = s.Store.PresignDownload(ctx, bundleKey, d)
		out.FencingToken, out.LeaseExpiresUnix = lease.token, lease.expires.Unix()
		return err
	})
	if err != nil {
		return nil, publicError(err)
	}
	return &out, nil
}

func descriptor(digest string, size int64) (*pb.ArtifactDescriptor, error) {
	raw, err := hex.DecodeString(digest)
	if err != nil || len(raw) != 32 || size < 0 {
		return nil, errors.New("invalid stored descriptor")
	}
	return &pb.ArtifactDescriptor{Sha256: raw, SizeBytes: uint64(size)}, nil
}

type upload struct {
	id, kind, digest string
	size             int64
	complete         bool
}

func (s *ArtifactControl) CreateArtifactUpload(ctx context.Context, req *pb.CreateArtifactUploadRequest) (*pb.ArtifactUploadTarget, error) {
	d := req.GetArtifact()
	switch req.GetKind() {
	case "build_log", "runner_log", "test_io", "custom", "metrics":
	default:
		return nil, status.Error(codes.InvalidArgument, "unsupported artifact kind")
	}
	if !canonicalUUID(req.GetClientArtifactId()) || len(d.GetSha256()) != 32 || d.GetSizeBytes() > 256<<20 {
		return nil, status.Error(codes.InvalidArgument, "invalid artifact descriptor")
	}
	return s.grant(ctx, req.GetLease(), "", req)
}

func (s *ArtifactControl) RenewArtifactUpload(ctx context.Context, req *pb.RenewArtifactUploadRequest) (*pb.ArtifactUploadTarget, error) {
	if !canonicalUUID(req.GetArtifactId()) {
		return nil, status.Error(codes.InvalidArgument, "invalid artifact id")
	}
	return s.grant(ctx, req.GetLease(), req.GetArtifactId(), nil)
}

func loadUpload(ctx context.Context, tx pgx.Tx, lease *pb.RunLease, id string, byClient bool) (upload, error) {
	var u upload
	query := `SELECT id,kind,digest,size,completed_at IS NOT NULL FROM invoker_upload_grants WHERE attempt_id=$1 AND run_id=$2 AND `
	if byClient {
		query += `client_artifact_id=$3`
	} else {
		query += `id=$3`
	}
	err := tx.QueryRow(ctx, query, lease.AttemptId, lease.RunId, id).Scan(&u.id, &u.kind, &u.digest, &u.size, &u.complete)
	return u, err
}

func (s *ArtifactControl) grant(ctx context.Context, req *pb.RunLease, id string, create *pb.CreateArtifactUploadRequest) (*pb.ArtifactUploadTarget, error) {
	var out pb.ArtifactUploadTarget
	err := db.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		lease, err := lockLease(ctx, tx, req, false)
		if err != nil {
			return err
		}
		if create != nil {
			id = create.ClientArtifactId
		}
		u, err := loadUpload(ctx, tx, req, id, create != nil)
		if create != nil {
			digest, size := hex.EncodeToString(create.Artifact.Sha256), int64(create.Artifact.SizeBytes)
			if errors.Is(err, pgx.ErrNoRows) {
				var count, total int64
				if err := tx.QueryRow(ctx, `SELECT count(*),coalesce(sum(size),0)::bigint FROM invoker_upload_grants WHERE attempt_id=$1`, req.AttemptId).Scan(&count, &total); err != nil {
					return err
				}
				if count >= 256 || total+size > 256<<20 {
					return status.Error(codes.ResourceExhausted, "attempt artifact quota exceeded")
				}
				u = upload{kind: create.Kind, digest: digest, size: size}
				err = tx.QueryRow(ctx, `INSERT INTO invoker_upload_grants(client_artifact_id,attempt_id,run_id,kind,digest,size) VALUES($1,$2,$3,$4,$5,$6) RETURNING id`, id, req.AttemptId, req.RunId, u.kind, u.digest, u.size).Scan(&u.id)
			} else if err == nil && (u.digest != digest || u.size != size || u.kind != create.Kind) {
				return status.Error(codes.AlreadyExists, "artifact identity already bound")
			}
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return status.Error(codes.NotFound, "artifact grant not found")
		}
		if err != nil {
			return err
		}
		out.ArtifactId, out.Complete = u.id, u.complete
		if u.complete {
			return nil
		}
		d, err := ttl(lease.expires)
		if err != nil {
			return err
		}
		out.ExpiresAtUnixMillis = time.Now().Add(d).UnixMilli()
		out.PutUrl, err = s.Store.PresignUpload(ctx, "invoker/"+u.id, d, u.digest, u.size)
		raw, _ := hex.DecodeString(u.digest)
		out.RequiredHeaders = map[string]string{"x-amz-checksum-sha256": base64.StdEncoding.EncodeToString(raw), "content-length": strconv.FormatInt(u.size, 10)}
		return err
	})
	if err != nil {
		return nil, publicError(err)
	}
	return &out, nil
}

func (s *ArtifactControl) CompleteArtifactUpload(ctx context.Context, req *pb.CompleteArtifactUploadRequest) (*pb.Acknowledgement, error) {
	if !canonicalUUID(req.GetArtifactId()) {
		return nil, status.Error(codes.InvalidArgument, "invalid artifact id")
	}
	var out pb.Acknowledgement
	err := db.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		lease, err := lockLease(ctx, tx, req.GetLease(), false)
		if err != nil {
			return err
		}
		u, err := loadUpload(ctx, tx, req.Lease, req.ArtifactId, false)
		if errors.Is(err, pgx.ErrNoRows) {
			// A missing grant is not permission to retry PUT. Reserve NotFound
			// for a missing staging object belonging to a valid grant.
			return status.Error(codes.FailedPrecondition, "artifact grant unavailable")
		}
		if err != nil {
			return err
		}
		if u.complete {
			out.Duplicate = true
			return nil
		}
		bounded, cancel := context.WithDeadline(ctx, lease.expires)
		defer cancel()
		if err = s.Store.FinalizeUpload(bounded, tx, u.id, u.kind, "invoker/"+u.id, u.size, u.digest); err != nil {
			return err
		}
		if !time.Now().Before(lease.expires) {
			return status.Error(codes.FailedPrecondition, "run lease expired")
		}
		if _, err = tx.Exec(ctx, `INSERT INTO attempt_artifacts(attempt_id,artifact_id,kind) VALUES($1,$2,$3)`, req.Lease.AttemptId, u.id, u.kind); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE invoker_upload_grants SET completed_at=clock_timestamp() WHERE id=$1`, u.id)
		return err
	})
	if err != nil {
		return nil, publicError(err)
	}
	return &out, nil
}

func publicError(err error) error {
	if errors.Is(err, artifact.ErrUploadMissing) {
		return status.Error(codes.NotFound, "uploaded object not present")
	}
	if errors.Is(err, artifact.ErrUploadMismatch) {
		return status.Error(codes.DataLoss, "uploaded object does not match descriptor")
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return status.FromContextError(err).Err()
	}
	return status.Error(codes.Unavailable, "artifact operation unavailable")
}
