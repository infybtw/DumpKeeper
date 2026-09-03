package backup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"dumpkeeper/internal/db"
)

// Restore replays a completed backup into the job's own configured database
// via pg_restore. Local file is preferred; S3 is fetched to a temp file when
// the local copy is gone.
func (e *Engine) Restore(ctx context.Context, b db.Backup) error {
	if b.Status != db.StatusCompleted {
		return fmt.Errorf("backup %d is not completed, refusing restore", b.ID)
	}
	job, err := e.DB.GetJob(b.JobID)
	if err != nil {
		return fmt.Errorf("load job: %w", err)
	}

	path := filepath.Join(e.Local.Root, b.Filename)
	if _, err := os.Stat(path); err != nil {
		if !b.StoredS3 {
			return fmt.Errorf("dump %q not found locally and not stored in S3", b.Filename)
		}
		tmp, err := e.fetchFromS3(ctx, b.Filename)
		if err != nil {
			return err
		}
		path = tmp
		defer os.Remove(tmp)
	}

	cmd := exec.CommandContext(ctx, "pg_restore",
		"--host="+job.Host,
		"--port="+strconv.Itoa(job.Port),
		"--username="+job.Username,
		"--dbname="+job.DBName,
		"--clean", "--if-exists", "--no-owner", "--no-privileges", "--exit-on-error",
		path,
	)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+job.Password, "PGSSLMODE="+job.SSLMode)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := stderrTail(stderr.String(), stderrTailMax)
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("pg_restore: %s", detail)
	}

	restored := db.Now()
	b.RestoredAt = &restored
	return e.DB.UpdateBackup(b)
}

// fetchFromS3 streams the S3 object into a temp file and returns its path.
func (e *Engine) fetchFromS3(ctx context.Context, filename string) (string, error) {
	rc, _, err := e.S3.Open(ctx, filename)
	if err != nil {
		return "", fmt.Errorf("fetch %q from S3: %w", filename, err)
	}
	defer rc.Close()
	tmp, err := os.CreateTemp("", "dk-restore-*.dump")
	if err != nil {
		return "", fmt.Errorf("buffer S3 dump: %w", err)
	}
	if _, err := io.Copy(tmp, rc); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", fmt.Errorf("buffer S3 dump: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("buffer S3 dump: %w", err)
	}
	return tmp.Name(), nil
}
