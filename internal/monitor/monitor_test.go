package monitor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dumpkeeper/internal/db"
)

func TestPingWithRetries(t *testing.T) {
	dir := t.TempDir()
	attempts := filepath.Join(dir, "attempts")
	psql := filepath.Join(dir, "psql")
	if err := os.WriteFile(psql, []byte("#!/bin/sh\nprintf x >> \"$PING_ATTEMPTS\"\necho 'connection refused' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PING_ATTEMPTS", attempts)

	_, detail, err := pingWithRetries(context.Background(), db.Database{}, 5, time.Millisecond)
	if err == nil {
		t.Fatal("pingWithRetries unexpectedly succeeded")
	}
	if detail != "connection refused" {
		t.Fatalf("detail = %q, want connection refused", detail)
	}
	data, err := os.ReadFile(attempts)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "x"); got != 6 {
		t.Fatalf("probes = %d, want 6", got)
	}
}
