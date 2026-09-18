package scheduler

import (
	"sync"
	"testing"
	"time"

	"dumpkeeper/internal/db"
)

// TestRescheduleSkipsDisabledJobs proves the pause contract end to end: a
// disabled job with a live every-second schedule never fires, and enabling
// it re-registers the same schedule.
func TestRescheduleSkipsDisabledJobs(t *testing.T) {
	var mu sync.Mutex
	fired := 0
	s := New(func(jobID int64, trigger string) error {
		mu.Lock()
		fired++
		mu.Unlock()
		return nil
	})
	defer s.Stop()

	job := db.Job{ID: 1, Name: "paused", Schedule: "@every 1s", Enabled: false}
	s.Reschedule(job)
	time.Sleep(2500 * time.Millisecond)
	mu.Lock()
	n := fired
	mu.Unlock()
	if n != 0 {
		t.Fatalf("disabled job fired %d time(s), want 0", n)
	}

	job.Enabled = true
	s.Reschedule(job)
	for deadline := time.Now().Add(5 * time.Second); ; {
		mu.Lock()
		n = fired
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("re-enabled job never fired")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestRescheduleUsesJobTimezone(t *testing.T) {
	s := New(func(int64, string) error { return nil })
	defer s.Stop()

	s.Reschedule(db.Job{ID: 1, Name: "tokyo", Schedule: "0 9 * * *", Timezone: "Asia/Tokyo", Enabled: true})
	entry := s.c.Entry(s.entries[1])
	got := entry.Schedule.Next(time.Date(2026, time.January, 1, 23, 59, 0, 0, time.UTC))
	want := time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("next run = %s, want %s", got, want)
	}
}

func TestRescheduleUsesUTCOffsetTimezone(t *testing.T) {
	s := New(func(int64, string) error { return nil })
	defer s.Stop()

	s.Reschedule(db.Job{ID: 1, Name: "offset", Schedule: "0 9 * * *", Timezone: "UTC+3", Enabled: true})
	entry := s.c.Entry(s.entries[1])
	got := entry.Schedule.Next(time.Date(2026, time.January, 1, 5, 59, 0, 0, time.UTC))
	want := time.Date(2026, time.January, 1, 6, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("next run = %s, want %s", got, want)
	}
}

func TestNextRunUsesJobTimezone(t *testing.T) {
	job := db.Job{Schedule: "0 9 * * *", Timezone: "UTC+3", Enabled: true}
	got, ok := NextRun(job, time.Date(2026, time.January, 1, 5, 59, 0, 0, time.UTC))
	want := time.Date(2026, time.January, 1, 6, 0, 0, 0, time.UTC)
	if !ok || !got.Equal(want) {
		t.Fatalf("next run = %s, %t; want %s, true", got, ok, want)
	}
}
