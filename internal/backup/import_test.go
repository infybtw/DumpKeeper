package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRestoreCopyStripsOwnershipAndACLs(t *testing.T) {
	src := filepath.Join(t.TempDir(), "dump.sql")
	in := strings.Join([]string{
		"-- PostgreSQL database dump",
		`CREATE SCHEMA drizzle;`,
		`ALTER SCHEMA drizzle OWNER TO twn;`,
		`CREATE TABLE drizzle.notes(line text);`,
		`ALTER TABLE drizzle.notes OWNER TO twn;`,
		`COPY drizzle.notes (line) FROM stdin;`,
		`GRANT ALL ON TABLE stolen TO attacker;`, // COPY data, must survive
		`\.`,
		`GRANT USAGE ON SCHEMA drizzle TO twn;`,
		`REVOKE ALL ON SCHEMA drizzle FROM PUBLIC;`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE twn GRANT ALL ON TABLES TO twn;`,
		`INSERT INTO drizzle.notes VALUES ('after copy');`,
		"",
	}, "\n")
	if err := os.WriteFile(src, []byte(in), 0o644); err != nil {
		t.Fatal(err)
	}

	dst, cleanup, err := restoreCopy(src)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	out, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)

	for _, dropped := range []string{
		"OWNER TO",
		"GRANT USAGE",
		"REVOKE ALL",
		"ALTER DEFAULT PRIVILEGES",
	} {
		if strings.Contains(got, dropped) {
			t.Errorf("statement should be dropped: %q still present in output:\n%s", dropped, got)
		}
	}
	for _, kept := range []string{
		"CREATE SCHEMA drizzle;",
		"GRANT ALL ON TABLE stolen TO attacker;",           // COPY data survives
		"INSERT INTO drizzle.notes VALUES ('after copy');", // filtering resumes after \.
	} {
		if !strings.Contains(got, kept) {
			t.Errorf("line should be kept: %q missing from output:\n%s", kept, got)
		}
	}

	if again, err := os.ReadFile(src); err != nil || string(again) != in {
		t.Error("stored upload must stay untouched")
	}
}
