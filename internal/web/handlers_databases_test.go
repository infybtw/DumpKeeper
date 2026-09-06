package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"dumpkeeper/internal/db"
	"dumpkeeper/internal/monitor"
)

func TestDatabasePingUpdatesAvailability(t *testing.T) {
	dir := t.TempDir()
	failure := filepath.Join(dir, "offline")
	if err := os.WriteFile(failure, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "psql"), []byte("#!/bin/sh\nif [ -f \"$PING_FAILURE\" ]; then\n  echo 'connection refused' >&2\n  exit 1\nfi\nprintf '1\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PING_FAILURE", failure)
	store, err := db.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	dbe, err := store.CreateDatabase(db.Database{Name: "recovery", Host: "localhost", Port: 5432, DBName: "postgres", Username: "postgres", SSLMode: "disable"})
	if err != nil {
		t.Fatal(err)
	}
	mon := monitor.New(store)
	mon.Start()
	// Establish the outage through the background monitor, not the handler.
	deadline := time.Now().Add(5 * time.Second)
	for {
		states, err := store.ListPingStates()
		if err != nil {
			mon.Stop()
			t.Fatal(err)
		}
		if st, ok := states[dbe.ID]; ok && !st.OK {
			break
		}
		if time.Now().After(deadline) {
			mon.Stop()
			t.Fatal("background monitor did not record the outage")
		}
		time.Sleep(10 * time.Millisecond)
	}
	mon.Stop() // Manual checks must work without the background loop.
	s := &Server{db: store, mon: mon}

	check := func(wantUp bool, wantIncidents int) {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/databases/"+strconv.FormatInt(dbe.ID, 10)+"/ping", nil)
		r.SetPathValue("id", strconv.FormatInt(dbe.ID, 10))
		w := httptest.NewRecorder()
		s.databasePing(w, r)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("ping status = %d, body = %s", w.Code, w.Body.String())
		}
		data, err := s.dashboardData()
		if err != nil {
			t.Fatal(err)
		}
		wantStatus := "down"
		if wantUp {
			wantStatus = "up"
		}
		if len(data.Uptime) != 1 || data.Uptime[0].Status != wantStatus {
			t.Fatalf("dashboard availability = %+v, want %s", data.Uptime, wantStatus)
		}
		incidents, err := store.ListIncidents(10)
		if err != nil {
			t.Fatal(err)
		}
		if len(incidents) != wantIncidents {
			t.Fatalf("incidents = %+v, want %d periods", incidents, wantIncidents)
		}
		open := 0
		for _, inc := range incidents {
			if inc.EndedAt == nil {
				open++
			}
		}
		if (wantUp && open != 0) || (!wantUp && open != 1) {
			t.Fatalf("open incidents = %d, reachable = %t", open, wantUp)
		}
		states, err := store.ListPingStates()
		if err != nil {
			t.Fatal(err)
		}
		if wantUp && states[dbe.ID].Error != "" {
			t.Fatalf("recovered database retains error: %s", states[dbe.ID].Error)
		}
	}
	check(false, 1) // Repeated failures must not create another downtime period.
	if err := os.Remove(failure); err != nil {
		t.Fatal(err)
	}
	check(true, 1)
	check(true, 1) // Preserve closed history on subsequent successes.
	if err := os.WriteFile(failure, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	check(false, 2)
	if err := os.Remove(failure); err != nil {
		t.Fatal(err)
	}
	check(true, 2)
}
