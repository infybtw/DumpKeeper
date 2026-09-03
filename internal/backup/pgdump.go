package backup

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"dumpkeeper/internal/db"
)

// runDump runs pg_dump for job, writing a custom-format dump to path.
// Credentials travel via PGPASSWORD/PGSSLMODE so they never appear in argv.
func runDump(ctx context.Context, job db.Job, path string) error {
	cmd := exec.CommandContext(ctx, "pg_dump",
		"--format=custom",
		"--file="+path,
		"--host="+job.Host,
		"--port="+strconv.Itoa(job.Port),
		"--username="+job.Username,
		"--dbname="+job.DBName,
	)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+job.Password, "PGSSLMODE="+job.SSLMode)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := stderrTail(stderr.String(), stderrTailMax)
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("pg_dump: %s", detail)
	}
	return nil
}
