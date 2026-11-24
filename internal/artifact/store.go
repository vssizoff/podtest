package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/url"
	"os"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type Store struct {
	client *minio.Client
	bucket string
	pool   *pgxpool.Pool
}
type Ref struct {
	ID     string `json:"id"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

func New(pool *pgxpool.Pool) (*Store, error) {
	endpoint, err := url.Parse(os.Getenv("S3_URL"))
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return nil, errors.New("invalid S3_URL")
	}
	bucket, access, secret := os.Getenv("S3_BUCKET"), os.Getenv("S3_ACCESS_KEY"), os.Getenv("S3_SECRET_KEY")
	if bucket == "" || access == "" || secret == "" {
		return nil, errors.New("S3 bucket and credentials required")
	}
	client, err := minio.New(endpoint.Host, &minio.Options{Creds: credentials.NewStaticV4(access, secret, ""), Secure: endpoint.Scheme == "https", Region: "us-east-1"})
	if err != nil {
		return nil, err
	}
	return &Store{client, bucket, pool}, nil
}
func (s *Store) Init(ctx context.Context) error {
	exists, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil || exists {
		return err
	}
	return s.client.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{Region: "us-east-1"})
}
func (s *Store) Put(ctx context.Context, user, kind, media string, data []byte) (Ref, error) {
	return s.put(ctx, s.pool, user, kind, media, data)
}
func (s *Store) PutTx(ctx context.Context, tx pgx.Tx, user, kind, media string, data []byte) (Ref, error) {
	return s.put(ctx, tx, user, kind, media, data)
}

type executor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func (s *Store) put(ctx context.Context, exec executor, user, kind, media string, data []byte) (Ref, error) {
	id := uuid.NewString()
	key := kind + "/" + id
	sum := sha256.Sum256(data)
	ref := Ref{id, hex.EncodeToString(sum[:]), int64(len(data))}
	if _, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(data), ref.Size, minio.PutObjectOptions{ContentType: media}); err != nil {
		return Ref{}, err
	}
	_, err := exec.Exec(ctx, `INSERT INTO artifacts(id,storage_key,digest,size,media_type,kind,created_by) VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,'')::uuid)`, id, key, ref.Digest, ref.Size, media, kind, user)
	return ref, err
}
func (s *Store) Read(ctx context.Context, id string, limit int64) ([]byte, error) {
	reader, err := s.Open(ctx, id)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	b, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if int64(len(b)) > limit {
		return nil, errors.New("artifact exceeds read limit")
	}
	return b, err
}
func (s *Store) Open(ctx context.Context, id string) (io.ReadCloser, error) {
	var key string
	if err := s.pool.QueryRow(ctx, `SELECT storage_key FROM artifacts WHERE id=$1`, id).Scan(&key); err != nil {
		return nil, err
	}
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	if _, err = obj.Stat(); err != nil {
		obj.Close()
		return nil, err
	}
	return obj, nil
}
