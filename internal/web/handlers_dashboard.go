package web

import (
	"net/http"
	"strconv"
	"time"

	"dumpkeeper/internal/db"
)

// uptimeWindow is the period the per-database uptime percentage covers.
const uptimeWindow = 24 * time.Hour

// dashSegment is one slice of a dashboard donut. The ring has a 100-unit
// circumference (r=15.9155 in a 42-unit viewBox), so percentages are used
// directly as dash lengths.
type dashSegment struct {
	Pct    float64
	Rest   float64
	Offset float64
	Class  string // ok | err | warn | muted
}

// dashLegend is one legend row under a donut.
type dashLegend struct {
	Label string
	Value int64
	Class string
}

// dashCard is one summary card: headline count plus donut and legend, or a
// text note when there is nothing to chart.
type dashCard struct {
	Title    string
	Total    int64
	Segments []dashSegment
	Legend   []dashLegend
	Note     string
}

// uptimeRow is one row of the database uptime table.
type uptimeRow struct {
	Name      string
	Status    string // up | down | never
	Uptime    string
	Class     string // ok | warn | err | muted
	LastCheck string
	Latency   string
}

// recentExec is one row of the recent executions table.
type recentExec struct {
	Job      string
	Status   string
	Started  string
	Duration string
	Trigger  string
}

// dashboardData backs the dashboard page and its poll fragment.
type dashboardData struct {
	Cards  []dashCard
	Uptime []uptimeRow
	Recent []recentExec
}

func (s *Server) dashboardPage(w http.ResponseWriter, r *http.Request) {
	data, err := s.dashboardData()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.page(w, r, "dashboard.html", "Dashboard", http.StatusOK, data)
}

func (s *Server) dashboardFragment(w http.ResponseWriter, r *http.Request) {
	data, err := s.dashboardData()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderFragment(w, "fragment_dashboard.html", "dashboard-content", data)
}

// dashboardData gathers everything the summary page shows: donut cards,
// per-database uptime over the last 24h, and the latest executions.
func (s *Server) dashboardData() (dashboardData, error) {
	dbs, err := s.db.ListDatabases()
	if err != nil {
		return dashboardData{}, err
	}
	states, err := s.db.ListPingStates()
	if err != nil {
		return dashboardData{}, err
	}
	jobs, err := s.db.ListJobs()
	if err != nil {
		return dashboardData{}, err
	}
	counts, err := s.db.CountBackupsByStatus()
	if err != nil {
		return dashboardData{}, err
	}
	restored, lastRestored, err := s.db.RestorationStats()
	if err != nil {
		return dashboardData{}, err
	}
	incidents, err := s.db.ListIncidents(500)
	if err != nil {
		return dashboardData{}, err
	}
	backups, err := s.db.ListBackups(0, false)
	if err != nil {
		return dashboardData{}, err
	}

	// Databases card: current availability.
	var up, down, neverChecked int64
	for _, dbe := range dbs {
		if st, ok := states[dbe.ID]; ok {
			if st.OK {
				up++
			} else {
				down++
			}
		} else {
			neverChecked++
		}
	}
	dbCard := dashCard{
		Title: "Databases", Total: int64(len(dbs)),
		Segments: donutSegments([]dashPart{{up, "ok"}, {down, "err"}, {neverChecked, "muted"}}),
		Legend:   []dashLegend{{"up", up, "ok"}, {"down", down, "err"}, {"never", neverChecked, "muted"}},
	}

	// Jobs card: cron-scheduled vs manual-only.
	var scheduled, manual int64
	for _, j := range jobs {
		if j.Schedule != "" {
			scheduled++
		} else {
			manual++
		}
	}
	jobsCard := dashCard{
		Title: "Backup jobs", Total: int64(len(jobs)),
		Segments: donutSegments([]dashPart{{scheduled, "ok"}, {manual, "warn"}}),
		Legend:   []dashLegend{{"scheduled", scheduled, "ok"}, {"manual only", manual, "warn"}},
	}

	// Executions card: all-time status split.
	completed, failed, running := counts["completed"], counts["failed"], counts["running"]
	execCard := dashCard{
		Title: "Executions", Total: completed + failed + running,
		Segments: donutSegments([]dashPart{{completed, "ok"}, {failed, "err"}, {running, "warn"}}),
		Legend:   []dashLegend{{"completed", completed, "ok"}, {"failed", failed, "err"}, {"running", running, "warn"}},
	}

	restCard := dashCard{Title: "Restorations", Total: restored}
	if lastRestored == nil {
		restCard.Note = "No restorations yet"
	} else if t, err := db.ParseTime(*lastRestored); err == nil {
		restCard.Note = "Last: " + t.Local().Format("2006-01-02 15:04")
	} else {
		restCard.Note = "Last: unknown"
	}

	return dashboardData{
		Cards:  []dashCard{dbCard, jobsCard, execCard, restCard},
		Uptime: s.uptimeRows(dbs, states, incidents),
		Recent: recentExecs(backups, jobs),
	}, nil
}

// dashPart is one candidate donut slice before scaling to percentages.
type dashPart struct {
	value int64
	class string
}

// donutSegments turns counts into stacked donut slices; nil when empty.
func donutSegments(parts []dashPart) []dashSegment {
	var total int64
	for _, p := range parts {
		total += p.value
	}
	if total == 0 {
		return nil
	}
	var segs []dashSegment
	var acc float64
	for _, p := range parts {
		if p.value == 0 {
			continue
		}
		pct := 100 * float64(p.value) / float64(total)
		offset := 25 - acc
		if offset < 0 {
			offset += 100
		}
		segs = append(segs, dashSegment{
			Pct:    pct,
			Rest:   100 - pct,
			Offset: offset,
			Class:  p.class,
		})
		acc += pct
	}
	return segs
}

// uptimeRows computes each database's uptime percentage over the last 24h
// from incident overlap; databases never checked show a dash.
func (s *Server) uptimeRows(dbs []db.Database, states map[int64]db.PingState, incidents []db.Incident) []uptimeRow {
	now := time.Now()
	windowStart := now.Add(-uptimeWindow)
	byDB := map[int64][]db.Incident{}
	for _, inc := range incidents {
		byDB[inc.DatabaseID] = append(byDB[inc.DatabaseID], inc)
	}
	rows := make([]uptimeRow, 0, len(dbs))
	for _, dbe := range dbs {
		row := uptimeRow{Name: dbe.Name, Status: "never", Uptime: "—", Class: "muted"}
		st, checked := states[dbe.ID]
		if checked {
			row.Status = "down"
			row.Class = "err"
			if st.OK {
				row.Status = "up"
			}
			row.Latency = strconv.FormatInt(st.DurationMs, 10) + " ms"
			if t, err := db.ParseTime(st.CheckedAt); err == nil {
				row.LastCheck = t.Local().Format("2006-01-02 15:04:05")
			}
		}

		// Total observed time: since the profile was created if younger
		// than the window.
		start := windowStart
		if created, err := db.ParseTime(dbe.CreatedAt); err == nil && created.After(start) {
			start = created
		}
		total := now.Sub(start)
		if checked && total > 0 {
			var down time.Duration
			for _, inc := range byDB[dbe.ID] {
				from, err := db.ParseTime(inc.StartedAt)
				if err != nil {
					continue
				}
				to := now
				if inc.EndedAt != nil {
					if ended, err := db.ParseTime(*inc.EndedAt); err == nil {
						to = ended
					}
				}
				if from.Before(start) {
					from = start
				}
				if to.After(now) {
					to = now
				}
				if to.After(from) {
					down += to.Sub(from)
				}
			}
			pct := 100 * (1 - down.Seconds()/total.Seconds())
			row.Uptime = strconv.FormatFloat(pct, 'f', 1, 64) + "%"
			switch {
			case pct >= 99.5:
				row.Class = "ok"
			case pct >= 90:
				row.Class = "warn"
			default:
				row.Class = "err"
			}
			if !st.OK {
				row.Class = "err" // currently down: keep it red regardless
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// recentExecs renders the newest executions (backups arrive newest first).
func recentExecs(backups []db.Backup, jobs []db.Job) []recentExec {
	jobNames := make(map[int64]string, len(jobs))
	for _, j := range jobs {
		jobNames[j.ID] = j.Name
	}
	const limit = 8
	n := len(backups)
	if n > limit {
		n = limit
	}
	rows := make([]recentExec, 0, n)
	for _, b := range backups[:n] {
		row := recentExec{Job: jobNames[b.JobID], Status: b.Status, Trigger: b.Trigger}
		if t, err := db.ParseTime(b.StartedAt); err == nil {
			row.Started = t.Local().Format("2006-01-02 15:04:05")
			if b.FinishedAt != nil {
				if done, err := db.ParseTime(*b.FinishedAt); err == nil {
					row.Duration = formatDuration(done.Sub(t))
				}
			}
		}
		rows = append(rows, row)
	}
	return rows
}
