package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dumpkeeper/internal/db"
)

// settingsData is the display shape of the settings page.
type settingsData struct {
	IntervalMinutes string
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	iv := s.mon.Interval()
	s.page(w, r, "settings.html", "Settings", http.StatusOK, settingsData{
		IntervalMinutes: strconv.FormatInt(int64(iv/time.Minute), 10),
	})
}

func (s *Server) settingsSave(w http.ResponseWriter, r *http.Request) {
	minutes, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("interval_minutes")), 10, 64)
	if err != nil || minutes < 0 || minutes > 7*24*60 {
		s.redirectTo(w, r, "/settings", "", "Interval must be a whole number of minutes between 0 and 10080.")
		return
	}
	if err := s.mon.SetInterval(time.Duration(minutes) * time.Minute); err != nil {
		s.redirectTo(w, r, "/settings", "", "Could not save settings: "+err.Error())
		return
	}
	if minutes == 0 {
		s.redirectTo(w, r, "/settings", "Availability monitoring disabled.", "")
		return
	}
	s.redirectTo(w, r, "/settings",
		"Monitoring every "+strconv.FormatInt(minutes, 10)+" minute(s); first check runs now.", "")
}

// availabilityRow is the display shape of one availability-status row.
type availabilityRow struct {
	ID        int64
	Name      string
	Target    string
	Status    string // up | down | never
	CheckedAt string
	Latency   string
	Error     string
}

// incidentRow is the display shape of one downtime period.
type incidentRow struct {
	ID       int64
	DBName   string
	Started  string
	Ended    string // "" while ongoing
	Duration string
	Error    string
}

// availabilityData backs the availability page and its poll fragment.
type availabilityData struct {
	Enabled   bool
	Interval  string
	Rows      []availabilityRow
	Incidents []incidentRow
}

func (s *Server) availabilityPage(w http.ResponseWriter, r *http.Request) {
	data, err := s.availabilityRows()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	incidents, err := s.incidentRows()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	iv := s.mon.Interval()
	s.page(w, r, "availability.html", "Availability", http.StatusOK, availabilityData{
		Enabled:   iv > 0,
		Interval:  formatDuration(iv),
		Rows:      data,
		Incidents: incidents,
	})
}

func (s *Server) availabilityFragment(w http.ResponseWriter, r *http.Request) {
	rows, err := s.availabilityRows()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderFragment(w, "fragment_availability.html", "availability-rows", availabilityData{Rows: rows})
}

// availabilityRows joins databases with their latest probe result.
func (s *Server) availabilityRows() ([]availabilityRow, error) {
	dbs, err := s.db.ListDatabases()
	if err != nil {
		return nil, err
	}
	states, err := s.db.ListPingStates()
	if err != nil {
		return nil, err
	}
	rows := make([]availabilityRow, 0, len(dbs))
	for _, dbe := range dbs {
		row := availabilityRow{
			ID:     dbe.ID,
			Name:   dbe.Name,
			Target: fmt.Sprintf("%s:%d/%s", dbe.Host, dbe.Port, dbe.DBName),
			Status: "never",
		}
		if st, ok := states[dbe.ID]; ok {
			row.Status = "down"
			if st.OK {
				row.Status = "up"
			}
			row.Latency = strconv.FormatInt(st.DurationMs, 10) + " ms"
			row.Error = st.Error
			if t, err := db.ParseTime(st.CheckedAt); err == nil {
				row.CheckedAt = t.Local().Format("2006-01-02 15:04:05")
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// incidentRows renders downtime periods; ongoing ones show time so far.
func (s *Server) incidentRows() ([]incidentRow, error) {
	incidents, err := s.db.ListIncidents(200)
	if err != nil {
		return nil, err
	}
	rows := make([]incidentRow, 0, len(incidents))
	for _, inc := range incidents {
		row := incidentRow{ID: inc.ID, DBName: inc.DBName, Error: inc.Error}
		started, err := db.ParseTime(inc.StartedAt)
		if err != nil {
			continue
		}
		row.Started = started.Local().Format("2006-01-02 15:04:05")
		if inc.EndedAt == nil {
			row.Duration = formatDuration(time.Since(started)) + " (ongoing)"
		} else if ended, err := db.ParseTime(*inc.EndedAt); err == nil {
			row.Ended = ended.Local().Format("2006-01-02 15:04:05")
			row.Duration = formatDuration(ended.Sub(started))
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// formatDuration renders a duration compactly: 45s, 2m 05s, 3h 12m, 2d 04h.
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	days := int64(d / (24 * time.Hour))
	hours := int64(d/time.Hour) % 24
	minutes := int64(d/time.Minute) % 60
	seconds := int64(d/time.Second) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %02dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %02dm", hours, minutes)
	case minutes > 0:
		return fmt.Sprintf("%dm %02ds", minutes, seconds)
	default:
		return fmt.Sprintf("%ds", seconds)
	}
}
