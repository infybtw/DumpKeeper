package backup

import (
	"bytes"
	"context"
	"dumpkeeper/internal/db"
	"dumpkeeper/internal/storage"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// Restore replays a completed backup into its job's configured database via
// pg_restore. The local copy is preferred; otherwise stored S3 destinations
// are tried in order, streaming each to a temp file.
func (e *Engine) Restore(ctx context.Context, b db.Backup) error {
	if b.Status != db.StatusCompleted {
		return fmt.Errorf("backup %d is not completed, refusing restore", b.ID)
	}
	job, err := e.DB.GetJob(b.JobID)
	if err != nil {
		return fmt.Errorf("load job: %w", err)
	}
	dbe, err := e.DB.GetDatabase(job.DatabaseID)
	if err != nil {
		return fmt.Errorf("load database: %w", err)
	}

	path := filepath.Join(e.Local.Root, b.Filename)
	if _, err := os.Stat(path); err != nil {
		dests, err := e.DB.BackupDestinations(b.ID)
		if err != nil {
			return err
		}
		if len(dests) == 0 {
			return fmt.Errorf("dump %q not found locally and not stored on any destination", b.Filename)
		}
		var fetched string
		var lastErr error
		for _, d := range dests {
			fetched, err = e.fetchFromS3(ctx, S3Store(d), b.Filename)
			if err == nil {
				lastErr = nil
				break
			}
			lastErr = fmt.Errorf("destination %s: %w", d.Name, err)
		}
		if lastErr != nil {
			return fmt.Errorf("fetch %q: %w", b.Filename, lastErr)
		}
		path = fetched
		defer os.Remove(path)
	}

	if err := runRestore(ctx, dbe, path); err != nil {
		return err
	}

	restored := db.Now()
	b.RestoredAt = &restored
	return e.DB.UpdateBackup(b)
}

// runRestore replays the custom-format dump at path into dbe via
// pg_restore: drop and recreate existing objects, keep ownership and grants
// out, abort on the first error. Credentials travel via PGPASSWORD/PGSSLMODE
// so they never appear in argv.
func runRestore(ctx context.Context, dbe db.Database, path string) error {
	cmd := exec.CommandContext(ctx, "pg_restore",
		"--host="+dbe.Host,
		"--port="+strconv.Itoa(dbe.Port),
		"--username="+dbe.Username,
		"--dbname="+dbe.DBName,
		"--clean", "--if-exists", "--no-owner", "--no-privileges", "--exit-on-error",
		path,
	)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+dbe.Password, "PGSSLMODE="+dbe.SSLMode)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := stderrTail(stderr.String(), stderrTailMax)
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("pg_restore: %s", detail)
	}
	return nil
}

// fetchFromS3 streams the S3 object into a temp file and returns its path.
func (e *Engine) fetchFromS3(ctx context.Context, store *storage.S3, filename string) (string, error) {
	rc, _, err := store.Open(ctx, filename)
	if err != nil {
		return "", err
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
