// Package scheduler drives cron-scheduled backups, one cron entry per job.
package scheduler

import (
	"log/slog"
	"sync"

	"dumpkeeper/internal/backup"
	"dumpkeeper/internal/db"

	"github.com/robfig/cron/v3"
)

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

// Reschedule replaces the cron entry for job. An empty schedule removes the
// entry; an unparsable schedule is logged and skipped (the job form already
// validates schedules with cron.ParseStandard before persisting).
func (s *Scheduler) Reschedule(job db.Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeLocked(job.ID)
	if job.Schedule == "" {
		return
	}
	id, err := s.c.AddFunc(job.Schedule, func() {
		if err := s.trigger(job.ID, backup.TriggerCron); err != nil && err != backup.ErrAlreadyRunning {
			slog.Warn("scheduler: cron trigger failed", "job", job.Name, "err", err)
		}
	})
	if err != nil {
		slog.Warn("scheduler: invalid schedule, job stays manual", "job", job.Name, "schedule", job.Schedule, "err", err)
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
