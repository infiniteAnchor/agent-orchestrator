package sessionmanager

import (
	"context"
	"fmt"
	"os"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type taskWorkerSessionStore interface {
	CreateTaskWorkerSession(context.Context, domain.SessionRecord, string) (domain.SessionRecord, error)
}

func (m *Manager) createSpawnSession(ctx context.Context, cfg ports.SpawnConfig, rec domain.SessionRecord) (domain.SessionRecord, error) {
	if cfg.TaskAttemptID == "" {
		return m.store.CreateSession(ctx, rec)
	}
	store, ok := m.store.(taskWorkerSessionStore)
	if !ok {
		return domain.SessionRecord{}, fmt.Errorf("task worker session binding unavailable")
	}
	return store.CreateTaskWorkerSession(ctx, rec, cfg.TaskAttemptID)
}

// TaskWorkerAlive only observes an existing worker. Incomplete durable launch
// state is inconclusive: recovery must hold it instead of starting a duplicate.
func (m *Manager) TaskWorkerAlive(ctx context.Context, id domain.SessionID) (bool, error) {
	rec, ok, err := m.store.GetSession(ctx, id)
	if err != nil {
		return false, err
	}
	if !ok || rec.IsTerminated || rec.Kind != domain.KindWorker || rec.Metadata.WorkspacePath == "" {
		return false, fmt.Errorf("task worker %s has incomplete or terminated session state", id)
	}
	if domain.NormalizeSessionMode(rec.Mode) == domain.SessionModeChat {
		if m.chat == nil || !m.chat.HasLiveChatController(id) {
			return false, fmt.Errorf("task worker %s has no observable live Chat controller", id)
		}
		return true, nil
	}
	if rec.Metadata.RuntimeHandleID == "" {
		return false, fmt.Errorf("task worker %s has no runtime handle", id)
	}
	return m.runtime.IsAlive(ctx, runtimeHandle(rec.Metadata))
}

type taskWorkerSessionLookup interface {
	IsTaskWorkerSession(context.Context, domain.SessionID) (bool, error)
}

func (m *Manager) reconcileTaskWorker(ctx context.Context, rec domain.SessionRecord) (bool, error) {
	store, ok := m.store.(taskWorkerSessionLookup)
	if !ok {
		return false, nil
	}
	bound, err := store.IsTaskWorkerSession(ctx, rec.ID)
	if err != nil {
		return true, fmt.Errorf("reconcile %s task ownership: %w", rec.ID, err)
	}
	if !bound {
		return false, nil
	}
	if !rec.IsTerminated && rec.Kind == domain.KindWorker &&
		domain.NormalizeSessionMode(rec.Mode) == domain.SessionModeChat &&
		rec.Harness == domain.HarnessCodex && m.chat != nil && !m.chat.HasLiveChatController(rec.ID) {
		if rec.Metadata.ProviderConversationID == "" || rec.Metadata.ControllerGeneration == "" {
			return true, fmt.Errorf("reconcile %s task worker has incomplete Chat ownership", rec.ID)
		}
		info, err := os.Stat(rec.Metadata.WorkspacePath)
		if err != nil || !info.IsDir() {
			return true, fmt.Errorf("reconcile %s task worker workspace unavailable", rec.ID)
		}
		project, err := m.loadProject(ctx, rec.ProjectID)
		if err != nil {
			return true, err
		}
		// Attach the existing host only. Do not restore a workspace, replay a
		// prompt, or fall back to native resume in a replacement provider.
		_, err = m.resumeChatControllerWithPolicy(ctx, "reconcile task worker", rec, project, ports.WorkspaceInfo{
			Path: rec.Metadata.WorkspacePath, RepoPath: rec.Metadata.WorkspaceRepoPath, Branch: rec.Metadata.Branch,
		}, false, "", true)
		if err != nil {
			return true, err
		}
	}
	alive, err := m.TaskWorkerAlive(ctx, rec.ID)
	if err != nil {
		return true, fmt.Errorf("reconcile %s task worker: %w", rec.ID, err)
	}
	if !alive {
		return true, fmt.Errorf("reconcile %s task worker launch is inconclusive", rec.ID)
	}
	return true, nil
}
