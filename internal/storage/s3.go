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

// S3Config carries one S3 destination's settings.
type S3Config struct {
	Endpoint  string
	Region    string
	Bucket    string
	Prefix    string
	AccessKey string
	SecretKey string
	UseSSL    bool
}

func (c S3Config) validate() error {
	for _, m := range []struct{ name, value string }{
		{"endpoint", c.Endpoint},
		{"bucket", c.Bucket},
		{"access key", c.AccessKey},
		{"secret key", c.SecretKey},
	} {
		if strings.TrimSpace(m.value) == "" {
			return fmt.Errorf("S3 destination is not fully configured: missing %s", m.name)
		}
	}
	return nil
}

// S3 stores backup files in one S3-compatible bucket. A client is built per
// call so destination edits take effect immediately. Objects are stored as
// {prefix}/{filename}.
type S3 struct {
	cfg S3Config
}

var _ Store = (*S3)(nil)

// NewS3 returns an S3 store for the given destination config.
func NewS3(cfg S3Config) *S3 { return &S3{cfg: cfg} }

func (s *S3) client() (*minio.Client, error) {
	if err := s.cfg.validate(); err != nil {
		return nil, err
	}
	client, err := minio.New(s.cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(s.cfg.AccessKey, s.cfg.SecretKey, ""),
		Secure: s.cfg.UseSSL,
		Region: s.cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("S3 client: %w", err)
	}
	return client, nil
}

func (s *S3) objectName(filename string) string {
	if prefix := strings.Trim(s.cfg.Prefix, "/"); prefix != "" {
		return prefix + "/" + filename
	}
	return filename
}

// Put uploads srcPath to the bucket as prefix/filename.
func (s *S3) Put(ctx context.Context, filename, srcPath string) error {
	client, err := s.client()
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
	_, err = client.PutObject(ctx, s.cfg.Bucket, s.objectName(filename), f, info.Size(), minio.PutObjectOptions{})
	if err != nil {
		return fmt.Errorf("S3 put %s: %w", filename, err)
	}
	return nil
}

// Open downloads prefix/filename; the returned reader streams the object.
func (s *S3) Open(ctx context.Context, filename string) (io.ReadCloser, int64, error) {
	client, err := s.client()
	if err != nil {
		return nil, 0, err
	}
	obj, err := client.GetObject(ctx, s.cfg.Bucket, s.objectName(filename), minio.GetObjectOptions{})
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
	client, err := s.client()
	if err != nil {
		return err
	}
	err = client.RemoveObject(ctx, s.cfg.Bucket, s.objectName(filename), minio.RemoveObjectOptions{})
	if err != nil {
		return fmt.Errorf("S3 delete %s: %w", filename, err)
	}
	return nil
}
