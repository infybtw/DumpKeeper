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
