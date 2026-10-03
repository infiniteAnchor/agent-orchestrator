package tasksched

import (
	"context"
	"fmt"
	"sync"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// WorkerStore exposes durable launch ownership, including the session binding
// committed by session creation before any workspace or controller side effect.
type WorkerStore interface {
	GetTaskAttempt(context.Context, string) (domain.TaskAttempt, bool, error)
	GetSession(context.Context, domain.SessionID) (domain.SessionRecord, bool, error)
}

// WorkerSessions uses the normal daemon-owned session/workspace spawn path.
type WorkerSessions interface {
	Spawn(context.Context, ports.SpawnConfig) (domain.Session, int, int, error)
	WorkspaceLocation(context.Context, domain.SessionID) (string, error)
}

// WorkerProbe must preserve unknown runtime observations as errors.
type WorkerProbe interface {
	TaskWorkerAlive(context.Context, domain.SessionID) (bool, error)
}

// SessionLauncher binds one real, isolated worker session to each attempt.
// It never restores or resends a prompt during reconciliation. A partial seed,
// missing session, or uncertain controller holds the attempt for inspection.
type SessionLauncher struct {
	store    WorkerStore
	sessions WorkerSessions
	probe    WorkerProbe
	mu       sync.Mutex
}

// NewSessionLauncher wires durable session ownership and observation-only recovery.
func NewSessionLauncher(store WorkerStore, sessions WorkerSessions, probe WorkerProbe) *SessionLauncher {
	return &SessionLauncher{store: store, sessions: sessions, probe: probe}
}

// WorkspaceLocation resolves the worker directory used for candidate verification.
func (l *SessionLauncher) WorkspaceLocation(ctx context.Context, id domain.SessionID) (string, error) {
	return l.sessions.WorkspaceLocation(ctx, id)
}

// Dispatch starts an unbound leased attempt or adopts its existing worker.
func (l *SessionLauncher) Dispatch(ctx context.Context, req LaunchRequest) (LaunchOutcome, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	outcome, err := l.reconcile(ctx, req)
	if err != nil || outcome.Disposition != LaunchNotStarted {
		return outcome, err
	}
	// CreateTaskWorkerSession atomically binds the attempt and allocates its
	// session before Spawn can create a worktree or launch a provider. A competing
	// dispatch is refused by that transaction, even across launcher instances.
	_, _, _, err = l.sessions.Spawn(ctx, ports.SpawnConfig{
		ProjectID: domain.ProjectID(req.ProjectID), Kind: domain.KindWorker,
		Harness: req.Harness, Prompt: req.Prompt, TaskAttemptID: req.AttemptID,
	})
	if err != nil {
		// Spawn can fail after a controller accepted work; never turn an error into
		// proof of absence or automatically retry its prompt.
		return LaunchOutcome{Disposition: LaunchAmbiguous}, err
	}
	return l.reconcile(ctx, req)
}

// Reconcile observes durable ownership without starting or resuming a worker.
func (l *SessionLauncher) Reconcile(ctx context.Context, req LaunchRequest) (LaunchOutcome, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reconcile(ctx, req)
}

func (l *SessionLauncher) reconcile(ctx context.Context, req LaunchRequest) (LaunchOutcome, error) {
	ambiguous := LaunchOutcome{Disposition: LaunchAmbiguous}
	attempt, ok, err := l.store.GetTaskAttempt(ctx, req.AttemptID)
	if err != nil {
		return ambiguous, err
	}
	if !ok || attempt.PlanID != req.PlanID || attempt.TaskID != req.TaskID {
		return ambiguous, fmt.Errorf("task worker attempt identity mismatch")
	}
	if attempt.SessionID == "" {
		if attempt.State != domain.TaskAttemptStateClaimed || attempt.RuntimeRef != domain.TaskAttemptDispatchLease {
			return ambiguous, nil
		}
		return LaunchOutcome{Disposition: LaunchNotStarted}, nil
	}
	id := domain.SessionID(attempt.SessionID)
	rec, ok, err := l.store.GetSession(ctx, id)
	if err != nil {
		return ambiguous, err
	}
	if !ok || rec.ProjectID != domain.ProjectID(req.ProjectID) || rec.Kind != domain.KindWorker || rec.IsTerminated || (req.Harness != "" && rec.Harness != req.Harness) {
		return ambiguous, nil
	}
	if location, err := l.sessions.WorkspaceLocation(ctx, id); err != nil || location == "" {
		return ambiguous, nil //nolint:nilerr // An unavailable workspace is a task-local hold, not a failed recovery.
	}
	alive, err := l.probe.TaskWorkerAlive(ctx, id)
	if err != nil || !alive {
		return ambiguous, nil //nolint:nilerr // An unknown runtime observation holds this attempt while other work remains recoverable.
	}
	return LaunchOutcome{Disposition: LaunchStarted, RuntimeRef: "session:" + string(id), SessionID: string(id), Harness: rec.Harness}, nil
}
