package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"dumpkeeper/internal/db"
	"dumpkeeper/internal/monitor"
)

func TestDashboardShowsSelectedRangeAndChart(t *testing.T) {
	e := newMetricsEnv(t)
	e.saveExecution(t, "dashboard.sql", "SELECT 1;\n")
	database, err := e.store.CreateDatabase(db.Database{Name: "analytics", Host: "localhost", Port: 5432, Username: "postgres", DBName: "analytics"})
	if err != nil {
		t.Fatal(err)
	}
	job, err := e.store.CreateJob(db.Job{Name: "analytics-backup", DatabaseID: database.ID, DestLocal: true, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	id, err := e.store.CreateBackup(job.ID, db.StatusCompleted, "manual", db.Now(), "analytics.sql")
	if err != nil {
		t.Fatal(err)
	}
	backup, err := e.store.GetBackup(id)
	if err != nil {
		t.Fatal(err)
	}
	finished := db.FormatTime(time.Now())
	backup.FinishedAt, backup.RowCount = &finished, 128
	if err := e.store.UpdateBackup(backup); err != nil {
		t.Fatal(err)
	}

	w := e.get(t, "/?period=7d")
	if w.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, body = %s", w.Code, w.Body.String())
	}
	for _, want := range []string{
		`href="/?period=12h"`,
		`href="/?period=24h"`,
		`href="/?period=7d"`,
		`range-option is-active">7 days`,
		`hx-get="/fragment/dashboard?period=7d"`,
		"Backup execution trend",
		`class="execution-chart"`,
		"dashboard.js?v=",
		`data-tooltip=`,
		"completed, 0 failed",
		`class="chart-tick"`,
		`class="chart-label"`,
		`style="fill:var(--muted)"`,
		`style="fill:none;stroke:var(--ok)"`,
		"Database row trend",
		">analytics</h2>",
		"128 rows",
		"Database uptime (7 days)",
	} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("dashboard missing %q:\n%s", want, w.Body.String())
		}
	}
}

func TestDashboardHourlyRangesUseHourlyPoints(t *testing.T) {
	e := newMetricsEnv(t)
	for _, key := range []string{"12h", "24h"} {
		data, err := e.s.dashboardData(dashboardPeriodFor(key))
		if err != nil {
			t.Fatal(err)
		}
		want := 12
		if key == "24h" {
			want = 24
		}
		if len(data.Chart) != want {
			t.Errorf("%s chart points = %d, want %d", key, len(data.Chart), want)
		}
	}
}

func TestDashboardHidesDisabledPanels(t *testing.T) {
	e := newMetricsEnv(t)
	if err := e.store.SetSetting(db.SettingDashboardPanels, "executions"); err != nil {
		t.Fatal(err)
	}

	w := e.get(t, "/")
	if w.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, body = %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Backup execution trend") {
		t.Error("enabled execution trend is missing")
	}
	for _, hidden := range []string{"Database row trend", "Database uptime (", "Recent executions", `class="cards"`} {
		if strings.Contains(body, hidden) {
			t.Errorf("disabled dashboard panel still rendered: %q", hidden)
		}
	}
}

func TestSettingsShowsDashboardPanelPreferences(t *testing.T) {
	e := newMetricsEnv(t)
	if err := e.store.SetSetting(db.SettingDashboardPanels, "executions,rows"); err != nil {
		t.Fatal(err)
	}

	w := e.get(t, "/settings")
	if w.Code != http.StatusOK {
		t.Fatalf("settings status = %d, body = %s", w.Code, w.Body.String())
	}
	for _, want := range []string{
		"Dashboard panels",
		`value="executions" checked`,
		`value="rows" checked`,
		`value="summary" >`,
	} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("settings missing %q", want)
		}
	}
}

func TestSettingsSavePersistsDashboardPanels(t *testing.T) {
	e := newMetricsEnv(t)
	e.s.mon = monitor.New(e.store)
	form := url.Values{
		"csrf":             {"metrics-csrf"},
		"interval_minutes": {"15"},
		"dashboard_panel":  {"summary", "uptime"},
	}
	r := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(e.cookie)
	w := httptest.NewRecorder()
	e.s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("settings save status = %d, body = %s", w.Code, w.Body.String())
	}
	value, err := e.store.GetSetting(db.SettingDashboardPanels)
	if err != nil {
		t.Fatal(err)
	}
	if value != "summary,uptime" {
		t.Errorf("saved dashboard panels = %q, want summary,uptime", value)
	}
}
