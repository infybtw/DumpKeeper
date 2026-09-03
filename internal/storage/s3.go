package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3 stores backup files in an S3-compatible bucket. The client is built per
// call from the current global settings, so settings changes take effect
// without a restart. Objects are stored as {prefix}/{filename}.
type S3 struct {
	settings func() (map[string]string, error)
}

var _ Store = (*S3)(nil)

// NewS3 returns an S3 store reading its configuration through settings.
func NewS3(settings func() (map[string]string, error)) *S3 {
	return &S3{settings: settings}
}

// conn resolves the current settings and builds a client. It returns the
// client, bucket, and object-key prefix ("").
func (s *S3) conn() (*minio.Client, string, string, error) {
	st, err := s.settings()
	if err != nil {
		return nil, "", "", fmt.Errorf("load S3 settings: %w", err)
	}
	for _, key := range []string{"s3_endpoint", "s3_bucket", "s3_access_key", "s3_secret_key"} {
		if st[key] == "" {
			return nil, "", "", fmt.Errorf("S3 is not fully configured: missing setting %q", key)
		}
	}
	client, err := minio.New(st["s3_endpoint"], &minio.Options{
		Creds:  credentials.NewStaticV4(st["s3_access_key"], st["s3_secret_key"], ""),
		Secure: st["s3_use_ssl"] == "1",
		Region: st["s3_region"],
	})
	if err != nil {
		return nil, "", "", fmt.Errorf("S3 client: %w", err)
	}
	return client, st["s3_bucket"], strings.Trim(st["s3_prefix"], "/"), nil
}

func (s *S3) objectName(prefix, filename string) string {
	if prefix != "" {
		return prefix + "/" + filename
	}
	return filename
}

// Put uploads srcPath to the bucket as prefix/filename.
func (s *S3) Put(ctx context.Context, filename, srcPath string) error {
	client, bucket, prefix, err := s.conn()
	if err != nil {
		return err
	}
	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	_, err = client.PutObject(ctx, bucket, s.objectName(prefix, filename), f, info.Size(), minio.PutObjectOptions{})
	if err != nil {
		return fmt.Errorf("S3 put %s: %w", filename, err)
	}
	return nil
}

// Open downloads prefix/filename; the returned reader streams the object.
func (s *S3) Open(ctx context.Context, filename string) (io.ReadCloser, int64, error) {
	client, bucket, prefix, err := s.conn()
	if err != nil {
		return nil, 0, err
	}
	obj, err := client.GetObject(ctx, bucket, s.objectName(prefix, filename), minio.GetObjectOptions{})
	if err != nil {
		return nil, 0, fmt.Errorf("S3 open %s: %w", filename, err)
	}
	st, err := obj.Stat()
	if err != nil {
		obj.Close()
		return nil, 0, fmt.Errorf("S3 stat %s: %w", filename, err)
	}
	return obj, st.Size, nil
}

// Delete removes prefix/filename from the bucket.
func (s *S3) Delete(ctx context.Context, filename string) error {
	client, bucket, prefix, err := s.conn()
	if err != nil {
		return err
	}
	err = client.RemoveObject(ctx, bucket, s.objectName(prefix, filename), minio.RemoveObjectOptions{})
	if err != nil {
		return fmt.Errorf("S3 delete %s: %w", filename, err)
	}
	return nil
}
