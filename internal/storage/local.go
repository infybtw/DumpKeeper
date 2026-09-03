// Package storage persists immutable backup files by filename in local
// directories or S3-compatible object stores.
package storage

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

// Store is a destination for backup files, addressed by filename.
type Store interface {
	Put(ctx context.Context, filename, srcPath string) error
	Open(ctx context.Context, filename string) (io.ReadCloser, int64, error)
	Delete(ctx context.Context, filename string) error
}

// Local keeps backup files flat under Root.
type Local struct {
	Root string
}

var _ Store = (*Local)(nil)

// NewLocal returns a local store rooted at root (DATA_DIR/backups).
func NewLocal(root string) *Local { return &Local{Root: root} }

// Put moves srcPath to Root/filename, falling back to copy+delete when the
// rename crosses a filesystem boundary.
func (l *Local) Put(ctx context.Context, filename, srcPath string) error {
	dst := filepath.Join(l.Root, filename)
	if err := os.Rename(srcPath, dst); err == nil {
		return nil
	}
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	dstF, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dstF, src); err != nil {
		dstF.Close()
		os.Remove(dst)
		return err
	}
	if err := dstF.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	if err := os.Remove(srcPath); err != nil {
		slog.Warn("storage: remove source after copy", "file", srcPath, "err", err)
	}
	return nil
}

// Open returns a reader for Root/filename and its size.
func (l *Local) Open(ctx context.Context, filename string) (io.ReadCloser, int64, error) {
	f, err := os.Open(filepath.Join(l.Root, filename))
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}

// Delete removes Root/filename; a missing file counts as success.
func (l *Local) Delete(ctx context.Context, filename string) error {
	if err := os.Remove(filepath.Join(l.Root, filename)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
