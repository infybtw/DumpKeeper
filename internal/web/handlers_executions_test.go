package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"dumpkeeper/internal/backup"
	"dumpkeeper/internal/config"
	"dumpkeeper/internal/db"
	"dumpkeeper/internal/monitor"
	"dumpkeeper/internal/scheduler"
	"dumpkeeper/internal/storage"
)

// metricsEnv wires a Server over a throwaway store and local backup
// directory; requests go through Handler() with a real session cookie.
type metricsEnv struct {
	s       *Server
	store   *db.Store
	backups string
	cookie  *http.Cookie
}

func newMetricsEnv(t *testing.T) *metricsEnv {
	t.Helper()
	dir := t.TempDir()
	store, err := db.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	backups := filepath.Join(dir, "backups")
	if err := os.MkdirAll(backups, 0o755); err != nil {
		t.Fatal(err)
	}
	engine := backup.New(store, storage.NewLocal(backups))
	s := New(config.Config{Login: "admin", Password: "admin123"},
		store, engine, new(scheduler.Scheduler), new(monitor.Monitor))
	const token = "metrics-session-token"
	if err := store.CreateSession(token, "metrics-csrf", db.FormatTime(time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	return &metricsEnv{
		s: s, store: store, backups: backups,
		cookie: &http.Cookie{Name: sessionCookie, Value: token},
	}
}

// saveExecution records a completed local-only execution and stores its
// dump file, returning the execution id.
func (e *metricsEnv) saveExecution(t *testing.T, filename, dump string) int64 {
	t.Helper()
	id, err := e.store.CreateBackup(0, db.StatusCompleted, backup.TriggerImport, db.Now(), filename)
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.store.GetBackup(id)
	if err != nil {
		t.Fatal(err)
	}
	finished := db.Now()
	b.FinishedAt, b.StoredLocal, b.SizeBytes = &finished, true, int64(len(dump))
	if err := e.store.UpdateBackup(b); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.backups, filename), []byte(dump), 0o644); err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *metricsEnv) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.AddCookie(e.cookie)
	w := httptest.NewRecorder()
	e.s.Handler().ServeHTTP(w, r)
	return w
}

func TestExecutionMetricsModalCountsCopyTables(t *testing.T) {
	e := newMetricsEnv(t)
	dump := strings.Join([]string{
		"-- PostgreSQL database dump",
		`COPY public.metric_lines_a (id) FROM stdin;`,
		"1",
		"2",
		`\.`,
		`COPY public.metric_lines_b (id) FROM stdin;`,
		"1",
		"2",
		"3",
		`\.`,
		"",
	}, "\n")
	id := e.saveExecution(t, "metrics.sql", dump)

	w := e.get(t, "/executions/"+strconv.FormatInt(id, 10)+"/metrics")
	if w.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, body = %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"Backup metrics · 2 tables",
		`<span class="metric-num">2</span>`,
		`<span class="metric-label">tables with COPY data</span>`,
		"<td>public.metric_lines_a</td>",
		"<td>2</td>",
		"<td>public.metric_lines_b</td>",
		"<td>3</td>",
		`data-modal-close`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics modal missing %q:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{"unavailable", "No COPY"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("metrics modal must not contain %q:\n%s", unwanted, body)
		}
	}
}

func TestExecutionMetricsModalWithoutCopyData(t *testing.T) {
	e := newMetricsEnv(t)
	id := e.saveExecution(t, "schema-only.sql",
		"-- PostgreSQL database dump\n\nSET statement_timeout = 0;\n")

	w := e.get(t, "/executions/"+strconv.FormatInt(id, 10)+"/metrics")
	if w.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, body = %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "No COPY table data found.") {
		t.Errorf("schema-only modal missing empty-data state:\n%s", body)
	}
	if strings.Contains(body, "unavailable") {
		t.Errorf("valid dump must not report unavailability:\n%s", body)
	}
}

func TestExecutionMetricsUnavailableWhenFileMissing(t *testing.T) {
	e := newMetricsEnv(t)
	id := e.saveExecution(t, "missing.sql", "COPY public.t (id) FROM stdin;\n1\n\\.\n")
	if err := os.Remove(filepath.Join(e.backups, "missing.sql")); err != nil {
		t.Fatal(err)
	}

	w := e.get(t, "/executions/"+strconv.FormatInt(id, 10)+"/metrics")
	if w.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, body = %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Metrics are unavailable for this backup.") {
		t.Errorf("missing-file modal missing unavailable state:\n%s", body)
	}
	for _, leaked := range []string{"no such file", "metric_lines", "public.t"} {
		if strings.Contains(body, leaked) {
			t.Errorf("storage error or partial data leaked into modal (%q):\n%s", leaked, body)
		}
	}
}

func TestExecutionsPageShowsInfoActionOnlyForSavedFiles(t *testing.T) {
	e := newMetricsEnv(t)
	with := e.saveExecution(t, "with.sql", "COPY public.t (id) FROM stdin;\n1\n\\.\n")
	without, err := e.store.CreateBackup(0, db.StatusFailed, backup.TriggerManual, db.Now(), "nowhere.sql")
	if err != nil {
		t.Fatal(err)
	}

	w := e.get(t, "/executions")
	if w.Code != http.StatusOK {
		t.Fatalf("executions status = %d, body = %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `hx-get="/executions/`+strconv.FormatInt(with, 10)+`/metrics"`) {
		t.Errorf("row with a saved file missing Info action:\n%s", body)
	}
	if strings.Contains(body, strconv.FormatInt(without, 10)+`/metrics`) {
		t.Errorf("row without a saved file must not get an Info action:\n%s", body)
	}
}

func TestExecutionMetricsRouteGuards(t *testing.T) {
	e := newMetricsEnv(t)
	for _, path := range []string{"/executions/999999/metrics", "/executions/abc/metrics"} {
		if w := e.get(t, path); w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, w.Code)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/executions/1/metrics", nil)
	w := httptest.NewRecorder()
	e.s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Errorf("unauthenticated metrics = %d, want redirect to login", w.Code)
	}
}
