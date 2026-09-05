package backup

import (
	"bytes"
	"context"
	"dumpkeeper/internal/db"
	"dumpkeeper/internal/storage"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Restore replays a completed backup into its job's configured database.
// Plain-text .sql dumps are streamed into psql with live byte progress;
// legacy custom-format .dump files still go through pg_restore. The local
// copy is preferred; otherwise stored S3 destinations are tried in order,
// streaming each to a temp file. Progress state is registered under the
// backup id and left terminal on return, so the executions row can show the
// outcome even though the row's stored status never changes.
func (e *Engine) Restore(ctx context.Context, b db.Backup) error {
	p := e.startProgress(b.ID, "preparing")
	fail := func(err error) error {
		p.finish(err.Error())
		return err
	}
	if b.Status != db.StatusCompleted {
		return fail(fmt.Errorf("backup %d is not completed, refusing restore", b.ID))
	}
	job, err := e.DB.GetJob(b.JobID)
	if err != nil {
		return fail(fmt.Errorf("load job: %w", err))
	}
	dbe, err := e.DB.GetDatabase(job.DatabaseID)
	if err != nil {
		return fail(fmt.Errorf("load database: %w", err))
	}

	path := filepath.Join(e.Local.Root, b.Filename)
	if _, err := os.Stat(path); err != nil {
		dests, err := e.DB.BackupDestinations(b.ID)
		if err != nil {
			return fail(err)
		}
		if len(dests) == 0 {
			return fail(fmt.Errorf("dump %q not found locally and not stored on any destination", b.Filename))
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
			return fail(fmt.Errorf("fetch %q: %w", b.Filename, lastErr))
		}
		path = fetched
		defer os.Remove(path)
	}

	if err := runRestore(ctx, dbe, path, false, p); err != nil {
		return fail(err)
	}

	p.finish("")
	restored := db.Now()
	b.RestoredAt = &restored
	return e.DB.UpdateBackup(b)
}

// RestoreAsync runs Restore in an engine-tracked goroutine and returns
// immediately; live progress shows up on the backup's executions row. It
// fails fast when this backup is already restoring or an import/restore is
// in flight.
func (e *Engine) RestoreAsync(b db.Backup) error {
	if _, ok := e.progress.Load(b.ID); ok {
		return fmt.Errorf("restore of %q is already running", b.Filename)
	}
	if !e.importMu.TryLock() {
		return errors.New("another import or restore is already running")
	}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer e.importMu.Unlock()
		if err := e.Restore(context.Background(), b); err != nil {
			slog.Error("restore", "backup", b.ID, "file", b.Filename, "err", err)
		}
	}()
	return nil
}

// dropDatabase drops dbe's database on its server, force-closing remaining
// connections (--force needs PostgreSQL 13+ on the server).
func dropDatabase(ctx context.Context, dbe db.Database) error {
	cmd := exec.CommandContext(ctx, "dropdb",
		"--host="+dbe.Host,
		"--port="+strconv.Itoa(dbe.Port),
		"--username="+dbe.Username,
		"--force",
		dbe.DBName,
	)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+dbe.Password, "PGSSLMODE="+dbe.SSLMode)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := stderrTail(stderr.String(), stderrTailMax)
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("dropdb: %s", detail)
	}
	return nil
}

// runRestore replays the dump at path into dbe, dispatching on the file
// extension: plain-text .sql goes through psql (streamed, byte progress via
// p), legacy custom-format .dump through pg_restore (indeterminate). Both
// drop and recreate existing objects (--clean is baked into our .sql dumps /
// passed here) and abort on the first error (ON_ERROR_STOP /
// --exit-on-error). Unless keepACLs is set, ownership and privilege
// statements are dropped (--no-owner --no-privileges / filtered out of
// .sql). Credentials travel via PGPASSWORD/PGSSLMODE so they never appear
// in argv.
func runRestore(ctx context.Context, dbe db.Database, path string, keepACLs bool, p *liveProgress) error {
	if filepath.Ext(path) == ".dump" {
		if p != nil {
			p.setPhase("restoring")
		}
		return runPgRestore(ctx, dbe, path, keepACLs)
	}
	return runPsql(ctx, dbe, path, p)
}

// runPsql replays a plain-text SQL dump with psql: --no-psqlrc skips the
// user's startup file, ON_ERROR_STOP aborts on the first error. The dump is
// streamed into stdin so p reports bytes actually handed to psql — the pipe
// back-pressures the feeder to psql's consumption rate, keeping the
// progress bar honest.
func runPsql(ctx context.Context, dbe db.Database, path string, p *liveProgress) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if info, err := f.Stat(); err == nil && p != nil {
		p.setPhase("restoring")
		p.setTotal(info.Size())
	}
	cmd := exec.CommandContext(ctx, "psql",
		"--host="+dbe.Host,
		"--port="+strconv.Itoa(dbe.Port),
		"--username="+dbe.Username,
		"--dbname="+dbe.DBName,
		"--no-psqlrc", "--set=ON_ERROR_STOP=1",
	)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+dbe.Password, "PGSSLMODE="+dbe.SSLMode)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard // command tags nobody reads; keep the pipe drained
	if err := cmd.Start(); err != nil {
		return err
	}

	buf := make([]byte, 256<<10)
feed:
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			if _, werr := stdin.Write(buf[:n]); werr != nil {
				// psql exited early and closed the pipe; Wait below surfaces
				// the real stderr.
				break feed
			}
			if p != nil {
				p.addDone(int64(n))
			}
		}
		switch {
		case rerr == io.EOF:
			break feed
		case rerr != nil:
			stdin.Close()
			cmd.Wait()
			return rerr
		}
	}
	stdin.Close()
	if err := cmd.Wait(); err != nil {
		detail := stderrTail(stderr.String(), stderrTailMax)
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("psql: %s", detail)
	}
	return nil
}

// databaseExists reports whether dbe's database exists on its server,
// asking via the maintenance "postgres" database. A single quote inside the
// name is escaped by doubling it, which is all a standard literal needs.
func databaseExists(ctx context.Context, dbe db.Database) (bool, error) {
	cmd := exec.CommandContext(ctx, "psql",
		"--host="+dbe.Host,
		"--port="+strconv.Itoa(dbe.Port),
		"--username="+dbe.Username,
		"--dbname=postgres",
		"--no-psqlrc", "--set=ON_ERROR_STOP=1",
		"--tuples-only", "--no-align",
		"--command=SELECT 1 FROM pg_database WHERE datname = '"+strings.ReplaceAll(dbe.DBName, "'", "''")+"'",
	)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+dbe.Password, "PGSSLMODE="+dbe.SSLMode)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		detail := stderrTail(stderr.String(), stderrTailMax)
		if detail == "" {
			detail = err.Error()
		}
		return false, fmt.Errorf("psql: %s", detail)
	}
	return strings.TrimSpace(string(out)) == "1", nil
}

// createDatabase creates dbe's database from template0, matching what
// pg_dump --create emits (any dump encoding/locale restores cleanly). The
// name is argv, so createdb quotes it as an identifier itself.
func createDatabase(ctx context.Context, dbe db.Database) error {
	cmd := exec.CommandContext(ctx, "createdb",
		"--host="+dbe.Host,
		"--port="+strconv.Itoa(dbe.Port),
		"--username="+dbe.Username,
		"--template=template0",
		dbe.DBName,
	)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+dbe.Password, "PGSSLMODE="+dbe.SSLMode)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := stderrTail(stderr.String(), stderrTailMax)
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("createdb: %s", detail)
	}
	return nil
}

// runPgRestore replays a legacy custom-format dump via pg_restore: drop and
// recreate existing objects, abort on the first error. Ownership and grants
// are dropped unless keepACLs is set.
func runPgRestore(ctx context.Context, dbe db.Database, path string, keepACLs bool) error {
	args := []string{
		"--host=" + dbe.Host,
		"--port=" + strconv.Itoa(dbe.Port),
		"--username=" + dbe.Username,
		"--dbname=" + dbe.DBName,
		"--clean", "--if-exists", "--exit-on-error",
	}
	if !keepACLs {
		args = append(args, "--no-owner", "--no-privileges")
	}
	cmd := exec.CommandContext(ctx, "pg_restore", append(args, path)...)
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
	tmp, err := os.CreateTemp("", "dk-restore-*"+filepath.Ext(filename))
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
