package web

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dumpkeeper/internal/db"
)

type dashboardPeriod struct {
	Key   string
	Label string
	Span  time.Duration
}

var dashboardPeriods = []dashboardPeriod{
	{Key: "12h", Label: "12 hours", Span: 12 * time.Hour},
	{Key: "24h", Label: "24 hours", Span: 24 * time.Hour},
	{Key: "7d", Label: "7 days", Span: 7 * 24 * time.Hour},
	{Key: "30d", Label: "30 days", Span: 30 * 24 * time.Hour},
	{Key: "90d", Label: "90 days", Span: 90 * 24 * time.Hour},
}

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

// executionChartPoint is one time bucket in the execution history chart.
type executionChartPoint struct {
	Label      string
	X          float64
	CompletedY float64
	FailedY    float64
	Completed  int64
	Failed     int64
}

// chartTick is one labelled position on a chart axis. Grid marks the value
// lines that draw a horizontal gridline (the zero baseline is drawn as the
// axis instead).
type chartTick struct {
	Label string
	Pos   float64
	Grid  bool
}

// databaseTrendPoint is one backup metric in a database's history.
type databaseTrendPoint struct {
	Label string
	X     float64
	Y     float64
	Value int64
}

type databaseTrendChart struct {
	Name    string
	Points  []databaseTrendPoint
	Path    string
	YTicks  []chartTick
	XTicks  []chartTick
	HasData bool
}

// dashboardData backs the dashboard page and its poll fragment.
type dashboardData struct {
	Panels         dashboardPanels
	Cards          []dashCard
	Uptime         []uptimeRow
	Recent         []recentExec
	Period         string
	PeriodLabel    string
	Periods        []dashboardPeriod
	Chart          []executionChartPoint
	ChartYTicks    []chartTick
	ChartXTicks    []chartTick
	CompletedPath  string
	FailedPath     string
	DatabaseTrends []databaseTrendChart
}

func (s *Server) dashboardPage(w http.ResponseWriter, r *http.Request) {
	data, err := s.dashboardData(dashboardPeriodFor(r.URL.Query().Get("period")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.page(w, r, "dashboard.html", "Dashboard", http.StatusOK, data)
}

func (s *Server) dashboardFragment(w http.ResponseWriter, r *http.Request) {
	data, err := s.dashboardData(dashboardPeriodFor(r.URL.Query().Get("period")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderFragment(w, "fragment_dashboard.html", "dashboard-content", data)
}

// dashboardData gathers everything the summary page shows for period.
func (s *Server) dashboardData(period dashboardPeriod) (dashboardData, error) {
	panels, err := s.dashboardPanels()
	if err != nil {
		return dashboardData{}, err
	}
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

	// Executions card: selected-period status split.
	periodBackups := backupsSince(backups, time.Now().Add(-period.Span))
	periodCounts := backupStatusCounts(periodBackups)
	completed, failed, running := periodCounts["completed"], periodCounts["failed"], periodCounts["running"]
	deleted := periodCounts["deleted"]
	execCard := dashCard{
		Title: "Executions", Total: completed + failed + running + deleted, Note: "Last " + period.Label,
		Segments: donutSegments([]dashPart{{completed, "ok"}, {failed, "err"}, {running, "warn"}, {deleted, "muted"}}),
		Legend:   []dashLegend{{"completed", completed, "ok"}, {"failed", failed, "err"}, {"running", running, "warn"}, {"deleted", deleted, "muted"}},
	}

	restCard := dashCard{Title: "Restorations", Total: restored}
	if lastRestored == nil {
		restCard.Note = "No restorations yet"
	} else if t, err := db.ParseTime(*lastRestored); err == nil {
		restCard.Note = "Last: " + t.Local().Format("2006-01-02 15:04")
	} else {
		restCard.Note = "Last: unknown"
	}

	chart, completedPath, failedPath := executionChart(periodBackups, period)
	rowCharts := databaseRowTrends(dbs, jobs, periodBackups, period)
	var chartMax int64 = 1
	for _, point := range chart {
		if total := point.Completed + point.Failed; total > chartMax {
			chartMax = total
		}
	}
	return dashboardData{
		Panels:         panels,
		Cards:          []dashCard{dbCard, jobsCard, execCard, restCard},
		Uptime:         s.uptimeRows(dbs, states, incidents, period.Span),
		Recent:         recentExecs(backups, jobs),
		Period:         period.Key,
		PeriodLabel:    period.Label,
		Periods:        dashboardPeriods,
		Chart:          chart,
		ChartYTicks:    yAxisTicks(chartMax),
		ChartXTicks:    xAxisTicks(chart),
		CompletedPath:  completedPath,
		FailedPath:     failedPath,
		DatabaseTrends: rowCharts,
	}, nil
}

// yAxisTicks labels the horizontal grid lines plus the zero baseline. Small
// maxima use one line per value so counts stay whole numbers; larger maxima
// fall back to four evenly spaced lines.
func yAxisTicks(max int64) []chartTick {
	if max < 1 {
		max = 1
	}
	const top, bottom = 38.0, 170.0
	steps := 4
	if max <= 4 {
		steps = int(max)
	}
	ticks := make([]chartTick, 0, steps+1)
	for i := 0; i <= steps; i++ {
		value := float64(max) * (1 - float64(i)/float64(steps))
		ticks = append(ticks, chartTick{
			Label: formatAxisValue(value),
			Pos:   top + (bottom-top)*float64(i)/float64(steps),
			Grid:  i < steps,
		})
	}
	return ticks
}

// xAxisTicks picks evenly spaced time labels along the X axis.
func xAxisTicks(points []executionChartPoint) []chartTick {
	if len(points) == 0 {
		return nil
	}
	const count = 5
	ticks := make([]chartTick, 0, count)
	for i := 0; i < count; i++ {
		index := int(math.Round(float64(i) * float64(len(points)-1) / float64(count-1)))
		ticks = append(ticks, chartTick{Label: points[index].Label, Pos: points[index].X})
	}
	return ticks
}

// formatAxisValue renders a tick value compactly (1.2k, 3M) so long row totals
// stay readable in the narrow Y-axis gutter.
func formatAxisValue(v float64) string {
	switch abs := math.Abs(v); {
	case abs >= 1e9:
		return strconv.FormatFloat(v/1e9, 'g', 3, 64) + "B"
	case abs >= 1e6:
		return strconv.FormatFloat(v/1e6, 'g', 3, 64) + "M"
	case abs >= 1e3:
		return strconv.FormatFloat(v/1e3, 'g', 3, 64) + "k"
	case v == math.Trunc(v):
		return strconv.FormatInt(int64(v), 10)
	default:
		return strconv.FormatFloat(v, 'g', 2, 64)
	}
}

func dashboardPeriodFor(key string) dashboardPeriod {
	for _, period := range dashboardPeriods {
		if period.Key == key {
			return period
		}
	}
	return dashboardPeriods[3]
}

func chartBuckets(period dashboardPeriod) (time.Duration, int, string) {
	if period.Key == "12h" || period.Key == "24h" {
		return time.Hour, int(period.Span / time.Hour), "15:04"
	}
	if period.Key == "90d" {
		return 7 * 24 * time.Hour, 13, "Jan 2"
	}
	return 24 * time.Hour, int(period.Span / (24 * time.Hour)), "Jan 2"
}

func backupsSince(backups []db.Backup, since time.Time) []db.Backup {
	filtered := make([]db.Backup, 0, len(backups))
	for _, b := range backups {
		if started, err := db.ParseTime(b.StartedAt); err == nil && !started.Before(since) {
			filtered = append(filtered, b)
		}
	}
	return filtered
}

func backupStatusCounts(backups []db.Backup) map[string]int64 {
	counts := map[string]int64{}
	for _, b := range backups {
		counts[b.Status]++
	}
	return counts
}

// executionChart makes daily buckets for short ranges and weekly buckets for
// the 90-day view, keeping the chart readable even with a long history.
func executionChart(backups []db.Backup, period dashboardPeriod) ([]executionChartPoint, string, string) {
	step, buckets, labelFormat := chartBuckets(period)
	now := time.Now().Local()
	start := now.Truncate(step).Add(-time.Duration(buckets-1) * step)
	completed := make([]int64, buckets)
	failed := make([]int64, buckets)
	for _, b := range backups {
		started, err := db.ParseTime(b.StartedAt)
		if err != nil {
			continue
		}
		index := int(started.Local().Sub(start) / step)
		if index < 0 || index >= buckets {
			continue
		}
		if b.Status == db.StatusCompleted {
			completed[index]++
		} else if b.Status == db.StatusFailed {
			failed[index]++
		}
	}
	var max int64 = 1
	for i := range completed {
		if total := completed[i] + failed[i]; total > max {
			max = total
		}
	}
	points := make([]executionChartPoint, buckets)
	for i := range points {
		points[i] = executionChartPoint{
			Label:      start.Add(time.Duration(i) * step).Format(labelFormat),
			X:          54 + 360*float64(i)/float64(buckets-1),
			CompletedY: 170 - 132*float64(completed[i])/float64(max),
			FailedY:    170 - 132*float64(failed[i])/float64(max),
			Completed:  completed[i],
			Failed:     failed[i],
		}
	}
	return points, executionPath(points, func(p executionChartPoint) float64 { return p.CompletedY }), executionPath(points, func(p executionChartPoint) float64 { return p.FailedY })
}

func executionPath(points []executionChartPoint, y func(executionChartPoint) float64) string {
	var path strings.Builder
	for i, point := range points {
		if i == 0 {
			fmt.Fprintf(&path, "M %.2f %.2f", point.X, y(point))
		} else {
			fmt.Fprintf(&path, " L %.2f %.2f", point.X, y(point))
		}
	}
	return path.String()
}

// databaseRowTrends creates one time series per connected database. Multiple
// jobs targeting the same database share a series; the latest backup in each
// time bucket wins, so retries do not distort the trend.
func databaseRowTrends(dbs []db.Database, jobs []db.Job, backups []db.Backup, period dashboardPeriod) []databaseTrendChart {
	jobDatabase := make(map[int64]int64, len(jobs))
	for _, job := range jobs {
		jobDatabase[job.ID] = job.DatabaseID
	}
	step, buckets, labelFormat := chartBuckets(period)
	now := time.Now().Local()
	start := now.Truncate(step).Add(-time.Duration(buckets-1) * step)
	values := make(map[int64][]db.Backup, len(dbs))
	for _, backup := range backups {
		databaseID := jobDatabase[backup.JobID]
		if databaseID == 0 || backup.Status != db.StatusCompleted || backup.RowCount < 0 {
			continue
		}
		started, err := db.ParseTime(backup.StartedAt)
		if err != nil {
			continue
		}
		index := int(started.Local().Sub(start) / step)
		if index < 0 || index >= buckets {
			continue
		}
		series := values[databaseID]
		if len(series) == 0 {
			series = make([]db.Backup, buckets)
		}
		if series[index].StartedAt < backup.StartedAt {
			series[index] = backup
		}
		values[databaseID] = series
	}
	charts := make([]databaseTrendChart, 0, len(dbs))
	for _, database := range dbs {
		series := values[database.ID]
		var max int64 = 1
		for _, backup := range series {
			if backup.RowCount > max {
				max = backup.RowCount
			}
		}
		chart := databaseTrendChart{
			Name:   database.Name,
			YTicks: yAxisTicks(max),
			XTicks: bucketXTicks(start, step, buckets, labelFormat),
		}
		for i, backup := range series {
			if backup.StartedAt == "" {
				continue
			}
			chart.HasData = true
			chart.Points = append(chart.Points, databaseTrendPoint{
				Label: start.Add(time.Duration(i) * step).Format(labelFormat),
				X:     54 + 360*float64(i)/float64(maxInt(buckets-1, 1)),
				Y:     170 - 132*float64(backup.RowCount)/float64(max),
				Value: backup.RowCount,
			})
		}
		var path strings.Builder
		for i, point := range chart.Points {
			if i == 0 {
				fmt.Fprintf(&path, "M %.2f %.2f", point.X, point.Y)
			} else {
				fmt.Fprintf(&path, " L %.2f %.2f", point.X, point.Y)
			}
		}
		chart.Path = path.String()
		charts = append(charts, chart)
	}
	return charts
}

// bucketXTicks labels evenly spaced buckets along the X axis; unlike
// xAxisTicks it does not depend on which buckets actually carry data.
func bucketXTicks(start time.Time, step time.Duration, buckets int, format string) []chartTick {
	if buckets <= 0 {
		return nil
	}
	const count = 5
	ticks := make([]chartTick, 0, count)
	for i := 0; i < count; i++ {
		index := int(math.Round(float64(i) * float64(buckets-1) / float64(count-1)))
		ticks = append(ticks, chartTick{
			Label: start.Add(time.Duration(index) * step).Format(format),
			Pos:   54 + 360*float64(index)/float64(maxInt(buckets-1, 1)),
		})
	}
	return ticks
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
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

// uptimeRows computes each database's uptime percentage over window from
// incident overlap; databases never checked show a dash.
func (s *Server) uptimeRows(dbs []db.Database, states map[int64]db.PingState, incidents []db.Incident, window time.Duration) []uptimeRow {
	now := time.Now()
	windowStart := now.Add(-window)
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
