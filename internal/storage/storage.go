// Package storage wraps S3-compatible object storage (MinIO locally, S3/R2 in prod).
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/blacmusic/backend/internal/config"
)

// ErrNotFound is returned when an object does not exist.
var ErrNotFound = errors.New("storage: object not found")

// Store talks to object storage. Two clients are kept because the hostname the
// API uses (e.g. "minio:9000" inside Docker) can differ from the one mobile
// clients must use in presigned URLs (e.g. "localhost:9000" or a CDN host).
type Store struct {
	internal *minio.Client
	public   *minio.Client
	bucket   string
	region   string
}

// New builds a Store from config. Region is set explicitly so presigning never
// needs a network round trip.
func New(cfg config.Config) (*Store, error) {
	creds := credentials.NewStaticV4(cfg.S3AccessKey, cfg.S3SecretKey, "")
	internal, err := minio.New(cfg.S3Endpoint, &minio.Options{Creds: creds, Secure: cfg.S3UseSSL, Region: cfg.S3Region})
	if err != nil {
		return nil, fmt.Errorf("storage client: %w", err)
	}
	public, err := minio.New(cfg.S3PublicEndpoint, &minio.Options{Creds: creds, Secure: cfg.S3PublicUseSSL, Region: cfg.S3Region})
	if err != nil {
		return nil, fmt.Errorf("storage public client: %w", err)
	}
	return &Store{internal: internal, public: public, bucket: cfg.S3Bucket, region: cfg.S3Region}, nil
}

// EnsureBucket creates the media bucket if it does not exist.
func (s *Store) EnsureBucket(ctx context.Context) error {
	ok, err := s.internal.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("check bucket: %w", err)
	}
	if ok {
		return nil
	}
	if err := s.internal.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{Region: s.region}); err != nil {
		return fmt.Errorf("create bucket: %w", err)
	}
	return nil
}

// PresignGet returns a time-limited download URL reachable by clients.
func (s *Store) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	u, err := s.public.PresignedGetObject(ctx, s.bucket, key, ttl, nil)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

// Ping checks that storage is reachable (used by /readyz).
func (s *Store) Ping(ctx context.Context) error {
	_, err := s.internal.BucketExists(ctx, s.bucket)
	return err
}

// PresignPut returns a time-limited upload URL reachable by clients. The client
// sends the file with a plain HTTP PUT.
func (s *Store) PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error) {
	u, err := s.public.PresignedPutObject(ctx, s.bucket, key, ttl)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

// Stat returns the size of an object, or ErrNotFound.
func (s *Store) Stat(ctx context.Context, key string) (int64, error) {
	info, err := s.internal.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return 0, mapErr(err)
	}
	return info.Size, nil
}

// Download copies an object to a local file.
func (s *Store) Download(ctx context.Context, key, dst string) error {
	return mapErr(s.internal.FGetObject(ctx, s.bucket, key, dst, minio.GetObjectOptions{}))
}

// UploadFile stores a local file under key.
func (s *Store) UploadFile(ctx context.Context, key, src, contentType string) error {
	_, err := s.internal.FPutObject(ctx, s.bucket, key, src, minio.PutObjectOptions{ContentType: contentType})
	return err
}

// Remove deletes an object. Removing a missing object is not an error.
func (s *Store) Remove(ctx context.Context, key string) error {
	return s.internal.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}

func mapErr(err error) error {
	if err == nil {
		return nil
	}
	switch minio.ToErrorResponse(err).Code {
	case "NoSuchKey", "NotFound":
		return ErrNotFound
	}
	return err
}

// Open streams an object. The caller must close the reader. It returns
// ErrNotFound when the object does not exist.
func (s *Store) Open(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	obj, err := s.internal.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, 0, mapErr(err)
	}
	st, err := obj.Stat() // GetObject is lazy: a missing key shows up here
	if err != nil {
		obj.Close()
		return nil, 0, mapErr(err)
	}
	return obj, st.Size, nil
}
