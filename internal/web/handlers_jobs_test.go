package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"testing"

	"dumpkeeper/internal/db"
	"dumpkeeper/internal/scheduler"
)

// Job row actions are triggered from the jobs page, so their redirects must
// land back on /jobs instead of bouncing the user to the dashboard.
func TestJobActionsRedirectToJobsPage(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := &Server{db: store, sched: scheduler.New(func(int64, string) error { return nil })}
	defer s.sched.Stop()

	dbe, err := store.CreateDatabase(db.Database{Name: "recovery", Host: "localhost", Port: 5432, DBName: "postgres", Username: "postgres", SSLMode: "disable"})
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.CreateJob(db.Job{Name: "nightly", DatabaseID: dbe.ID, KeepLast: 3})
	if err != nil {
		t.Fatal(err)
	}

	assertJobsRedirect := func(t *testing.T, w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != http.StatusSeeOther {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusSeeOther)
		}
		loc, err := url.Parse(w.Header().Get("Location"))
		if err != nil {
			t.Fatalf("parse Location: %v", err)
		}
		if loc.Path != "/jobs" {
			t.Errorf("redirect path = %q, want /jobs", loc.Path)
		}
	}

	t.Run("toggle", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/jobs/"+strconv.FormatInt(job.ID, 10)+"/toggle", nil)
		r.SetPathValue("id", strconv.FormatInt(job.ID, 10))
		w := httptest.NewRecorder()
		s.jobToggle(w, r)
		assertJobsRedirect(t, w)
	})

	t.Run("delete", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/jobs/"+strconv.FormatInt(job.ID, 10)+"/delete", nil)
		r.SetPathValue("id", strconv.FormatInt(job.ID, 10))
		w := httptest.NewRecorder()
		s.jobDelete(w, r)
		assertJobsRedirect(t, w)
	})
}
