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

// runDump runs pg_dump for the job's database, writing a custom-format dump
// to path. Credentials travel via PGPASSWORD/PGSSLMODE so they never appear
// in argv.
func runDump(ctx context.Context, dbe db.Database, path string) error {
	cmd := exec.CommandContext(ctx, "pg_dump",
		"--format=custom",
		"--file="+path,
		"--host="+dbe.Host,
		"--port="+strconv.Itoa(dbe.Port),
		"--username="+dbe.Username,
		"--dbname="+dbe.DBName,
	)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+dbe.Password, "PGSSLMODE="+dbe.SSLMode)
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
