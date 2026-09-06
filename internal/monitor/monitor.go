// Package monitor probes every configured database on an interval,
// records the latest result per database, and keeps one incident per
// downtime period (first failed ping -> first success after it).
package monitor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"dumpkeeper/internal/db"
)

// pingTimeout caps a single probe, exactly like the manual Ping button.
const pingTimeout = 10 * time.Second

// DefaultInterval is used when the ping_interval_seconds setting is absent.
const DefaultInterval = 15 * time.Minute

// Ping verifies the configured connection by authenticating and executing
// SELECT 1 via psql (pg_isready alone cannot validate credentials or dbname).
// It returns the probe latency, a human-readable error detail, and err.
func Ping(ctx context.Context, dbe db.Database) (time.Duration, string, error) {
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	start := time.Now()
	cmd := exec.CommandContext(ctx, "psql",
		"--no-password",
		"--tuples-only",
		"--no-align",
		"--quiet",
		"--host="+dbe.Host,
		"--port="+strconv.Itoa(dbe.Port),
		"--username="+dbe.Username,
		"--dbname="+dbe.DBName,
		"--command=SELECT 1",
	)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+dbe.Password, "PGSSLMODE="+dbe.SSLMode)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			detail = fmt.Sprintf("timed out after %s", pingTimeout)
		case detail == "":
			detail = err.Error()
		}
		return time.Since(start), detail, err
	}
	return time.Since(start), "", nil
}

// Monitor runs the probe loop. Interval <= 0 disables probing; SetInterval
// persists the setting and kicks the loop, which also probes immediately.
type Monitor struct {
	store    *db.Store
	mu       sync.Mutex
	interval time.Duration
	kick     chan struct{}
	stop     chan struct{}
	done     chan struct{}
	cancel   context.CancelFunc
}

// New loads the interval from the store and prepares (does not start) the
// loop.
func New(store *db.Store) *Monitor {
	m := &Monitor{
		store: store,
		kick:  make(chan struct{}, 1),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	m.interval = DefaultInterval
	if v, err := store.GetSetting(db.SettingPingInterval); err == nil {
		if secs, err := strconv.ParseInt(v, 10, 64); err == nil && secs >= 0 {
			m.interval = time.Duration(secs) * time.Second
		}
	} else if !errors.Is(err, db.ErrNotFound) {
		slog.Warn("monitor: read interval setting", "err", err)
	}
	return m
}

// Interval returns the current probe interval (0 = disabled).
func (m *Monitor) Interval() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.interval
}

// SetInterval persists the interval in seconds and applies it immediately.
func (m *Monitor) SetInterval(d time.Duration) error {
	secs := int64(d / time.Second)
	if err := m.store.SetSetting(db.SettingPingInterval, strconv.FormatInt(secs, 10)); err != nil {
		return err
	}
	m.mu.Lock()
	m.interval = d
	m.mu.Unlock()
	select {
	case m.kick <- struct{}{}:
	default:
	}
	return nil
}

// Start launches the probe loop; it probes once right away so status and
// incidents appear without waiting a full interval.
func (m *Monitor) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	go func() {
		defer close(m.done)
		m.loop(ctx)
	}()
}

// Stop cancels in-flight probes and waits for the loop to exit.
func (m *Monitor) Stop() {
	close(m.stop)
	if m.cancel != nil {
		m.cancel()
	}
	<-m.done
}

func (m *Monitor) loop(ctx context.Context) {
	for {
		if iv := m.Interval(); iv > 0 {
			m.pass(ctx)
		}
		iv := m.Interval()
		if iv <= 0 {
			iv = time.Hour // disabled: idle until SetInterval kicks
		}
		timer := time.NewTimer(iv)
		select {
		case <-m.stop:
			timer.Stop()
			return
		case <-m.kick:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// pass probes every database sequentially and updates state + incidents.
func (m *Monitor) pass(ctx context.Context) {
	dbs, err := m.store.ListDatabases()
	if err != nil {
		slog.Error("monitor: list databases", "err", err)
		return
	}
	for _, dbe := range dbs {
		if ctx.Err() != nil {
			return
		}
		if _, err := m.Check(ctx, dbe); err != nil && ctx.Err() == nil {
			slog.Error("monitor: check database", "database", dbe.Name, "err", err)
		}
	}
}

// Check probes a database and records its availability and downtime history.
// An unreachable database is returned as a failed PingState; err indicates
// cancellation or failure to persist the result.
func (m *Monitor) Check(ctx context.Context, dbe db.Database) (db.PingState, error) {
	latency, detail, err := Ping(ctx, dbe)
	if ctx.Err() != nil {
		return db.PingState{}, ctx.Err() // Do not record cancellation as downtime.
	}
	result := db.PingState{
		DatabaseID: dbe.ID,
		OK:         err == nil,
		CheckedAt:  db.Now(),
		DurationMs: latency.Milliseconds(),
		Error:      detail,
	}
	if err := m.store.RecordPingState(result); err != nil {
		return result, err
	}
	if !result.OK {
		slog.Warn("monitor: database unreachable", "database", dbe.Name, "err", detail)
	}
	return result, nil
}
