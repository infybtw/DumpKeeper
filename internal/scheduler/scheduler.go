// Package scheduler drives cron-scheduled backups, one cron entry per job.
package scheduler

import (
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"dumpkeeper/internal/backup"
	"dumpkeeper/internal/db"

	"github.com/robfig/cron/v3"
)

var utcOffset = regexp.MustCompile(`^UTC([+-])(\d{1,2})$`)

// cronTimezone returns a time zone identifier understood by robfig/cron.
// Besides IANA names, accept UTC+N and UTC-N as convenient fixed offsets.
// Etc/GMT intentionally uses the opposite sign (Etc/GMT-3 is UTC+03:00).
func cronTimezone(zone string) (string, error) {
	if _, err := time.LoadLocation(zone); err == nil {
		return zone, nil
	}
	matches := utcOffset.FindStringSubmatch(zone)
	if matches == nil {
		return "", fmt.Errorf("unknown time zone %q", zone)
	}
	hours, _ := strconv.Atoi(matches[2])
	if hours > 14 {
		return "", fmt.Errorf("UTC offset %q is outside -14 through +14", zone)
	}
	if hours == 0 {
		return "UTC", nil
	}
	sign := "+"
	if matches[1] == "+" {
		sign = "-"
	}
	return "Etc/GMT" + sign + strconv.Itoa(hours), nil
}

// CronTimezone returns the identifier used in a CRON_TZ expression.
func CronTimezone(zone string) (string, error) { return cronTimezone(zone) }

// NextRun returns the first scheduled run strictly after after. It uses the
// same cron expression and time-zone conversion as Scheduler.Reschedule.
func NextRun(job db.Job, after time.Time) (time.Time, bool) {
	if job.Schedule == "" || !job.Enabled {
		return time.Time{}, false
	}
	spec := job.Schedule
	if job.Timezone != "" {
		zone, err := cronTimezone(job.Timezone)
		if err != nil {
			return time.Time{}, false
		}
		spec = "CRON_TZ=" + zone + " " + spec
	}
	schedule, err := cron.ParseStandard(spec)
	if err != nil {
		return time.Time{}, false
	}
	return schedule.Next(after), true
}

// Scheduler wraps a cron.Cron and tracks jobID -> cron.EntryID so jobs can
// be rescheduled or removed individually.
type Scheduler struct {
	c       *cron.Cron
	mu      sync.Mutex
	entries map[int64]cron.EntryID
	trigger func(jobID int64, trigger string) error
}

// New starts a scheduler whose cron fires call trigger (normally
// Engine.Trigger, which serializes per job).
func New(trigger func(jobID int64, trigger string) error) *Scheduler {
	s := &Scheduler{
		c:       cron.New(),
		entries: make(map[int64]cron.EntryID),
		trigger: trigger,
	}
	s.c.Start()
	return s
}

// Reschedule replaces the cron entry for job. An empty schedule or a
// disabled job removes the entry; an unparsable schedule is logged and
// skipped (the job form already validates schedules with cron.ParseStandard
// before persisting).
func (s *Scheduler) Reschedule(job db.Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rescheduleLocked(job)
}

// Replace removes every scheduled job and schedules the supplied set. It is
// used after importing a configuration snapshot, whose job IDs may differ.
func (s *Scheduler) Replace(jobs []db.Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for jobID := range s.entries {
		s.removeLocked(jobID)
	}
	for _, job := range jobs {
		s.rescheduleLocked(job)
	}
}

func (s *Scheduler) rescheduleLocked(job db.Job) {
	s.removeLocked(job.ID)
	if job.Schedule == "" || !job.Enabled {
		return
	}
	spec := job.Schedule
	if job.Timezone != "" {
		zone, err := cronTimezone(job.Timezone)
		if err != nil {
			slog.Warn("scheduler: invalid time zone, job stays manual", "job", job.Name, "timezone", job.Timezone, "err", err)
			return
		}
		spec = "CRON_TZ=" + zone + " " + spec
	}
	id, err := s.c.AddFunc(spec, func() {
		if err := s.trigger(job.ID, backup.TriggerCron); err != nil && err != backup.ErrAlreadyRunning {
			slog.Warn("scheduler: cron trigger failed", "job", job.Name, "err", err)
		}
	})
	if err != nil {
		slog.Warn("scheduler: invalid schedule, job stays manual", "job", job.Name, "schedule", strings.TrimSpace(spec), "err", err)
		return
	}
	s.entries[job.ID] = id
}

// Remove cancels the cron entry for a job.
func (s *Scheduler) Remove(jobID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeLocked(jobID)
}

func (s *Scheduler) removeLocked(jobID int64) {
	if entry, ok := s.entries[jobID]; ok {
		s.c.Remove(entry)
		delete(s.entries, jobID)
	}
}

// Stop stops accepting new cron fires. A fire already running is a Trigger
// goroutine awaited via Engine.Wait at shutdown.
func (s *Scheduler) Stop() { s.c.Stop() }
