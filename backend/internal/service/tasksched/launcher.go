package tasksched

import (
	"context"
	"sync"
)

// MemoryLauncher is the attempt-scoped worker boundary used when no coding-agent
// session has been bound. It remembers every attempt it was asked to start so a
// later Dispatch or Reconcile in this process returns the same runtime ref.
// A launcher that has lost that memory reports the attempt as ambiguous: a
// missing entry is not proof the worker is absent.
type MemoryLauncher struct {
	mu         sync.Mutex
	refs       map[string]string
	alive      map[string]bool
	overrides  map[string]LaunchDisposition
	dispatches []string
}

// NewMemoryLauncher returns an empty in-process launcher.
func NewMemoryLauncher() *MemoryLauncher {
	return &MemoryLauncher{
		refs: map[string]string{}, alive: map[string]bool{}, overrides: map[string]LaunchDisposition{},
	}
}

// Remember records a worker identity created outside Dispatch, such as a launch
// that survived in an external registry across a scheduler restart.
func (l *MemoryLauncher) Remember(attemptID, runtimeRef string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refs[attemptID] = runtimeRef
	l.alive[attemptID] = true
}

// Override forces Reconcile to report a disposition for one attempt.
func (l *MemoryLauncher) Override(attemptID string, disposition LaunchDisposition) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.overrides[attemptID] = disposition
}

// Alive reports whether this process still considers the worker running.
// Completion does not clear it: a verified task may outlive its worker, and
// the scheduler must not need the worker to exit.
func (l *MemoryLauncher) Alive(attemptID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.alive[attemptID]
}

// DispatchCount is how many times Dispatch started (or reused) this attempt.
func (l *MemoryLauncher) DispatchCount(attemptID string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, id := range l.dispatches {
		if id == attemptID {
			n++
		}
	}
	return n
}

// Dispatch starts at most one logical worker per attempt.
func (l *MemoryLauncher) Dispatch(_ context.Context, req LaunchRequest) (LaunchOutcome, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.dispatches = append(l.dispatches, req.AttemptID)
	if ref, ok := l.refs[req.AttemptID]; ok {
		return LaunchOutcome{Disposition: LaunchStarted, RuntimeRef: ref, Harness: req.Harness}, nil
	}
	ref := "attempt:" + req.AttemptID
	l.refs[req.AttemptID] = ref
	l.alive[req.AttemptID] = true
	return LaunchOutcome{Disposition: LaunchStarted, RuntimeRef: ref, Harness: req.Harness}, nil
}

// Reconcile reports a remembered worker as started. Anything else is ambiguous
// unless a test override says the launcher confirmed the worker was not started.
func (l *MemoryLauncher) Reconcile(_ context.Context, req LaunchRequest) (LaunchOutcome, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if disposition, ok := l.overrides[req.AttemptID]; ok {
		ref := l.refs[req.AttemptID]
		return LaunchOutcome{Disposition: disposition, RuntimeRef: ref, Harness: req.Harness}, nil
	}
	if ref, ok := l.refs[req.AttemptID]; ok {
		return LaunchOutcome{Disposition: LaunchStarted, RuntimeRef: ref, Harness: req.Harness}, nil
	}
	return LaunchOutcome{Disposition: LaunchAmbiguous, Harness: req.Harness}, nil
}
