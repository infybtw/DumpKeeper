package backup

import "sync"

// RestoreProgress is the pollable state of one restore. Replayed/Total track
// bytes streamed into psql (Total 0 = indeterminate, the pg_restore path).
// Finished marks a terminal entry — with Error on failure. Terminal entries
// linger so an executions row can show how its restore ended until the row
// is deleted or the process restarts.
type RestoreProgress struct {
	Phase    string
	Replayed int64
	Total    int64
	Finished bool
	Error    string
}

// liveProgress is the engine-side, mutex-guarded half of RestoreProgress.
type liveProgress struct {
	mu    sync.Mutex
	phase string
	done  int64
	total int64
	fin   bool
	err   string
}

func (l *liveProgress) setPhase(phase string) {
	l.mu.Lock()
	l.phase = phase
	l.mu.Unlock()
}

func (l *liveProgress) setTotal(total int64) {
	l.mu.Lock()
	l.total = total
	l.mu.Unlock()
}

func (l *liveProgress) addDone(n int64) {
	l.mu.Lock()
	l.done += n
	l.mu.Unlock()
}

func (l *liveProgress) finish(errMsg string) {
	l.mu.Lock()
	l.fin, l.err = true, errMsg
	l.mu.Unlock()
}

func (l *liveProgress) snapshot() RestoreProgress {
	l.mu.Lock()
	defer l.mu.Unlock()
	return RestoreProgress{
		Phase: l.phase, Replayed: l.done, Total: l.total,
		Finished: l.fin, Error: l.err,
	}
}

// startProgress registers live restore state for id, replacing any old one.
func (e *Engine) startProgress(id int64, phase string) *liveProgress {
	l := &liveProgress{phase: phase}
	e.progress.Store(id, l)
	return l
}

// Progress snapshots the live restore state for id, if any.
func (e *Engine) Progress(id int64) (RestoreProgress, bool) {
	v, ok := e.progress.Load(id)
	if !ok {
		return RestoreProgress{}, false
	}
	return v.(*liveProgress).snapshot(), true
}

// ClearProgress drops the state for id (used when the row is deleted).
func (e *Engine) ClearProgress(id int64) { e.progress.Delete(id) }
