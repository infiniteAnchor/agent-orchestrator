package sessionmanager

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type bindingWorkerStore struct {
	*fakeStore
	attempt string
	bindErr error
}

func (s *bindingWorkerStore) CreateTaskWorkerSession(ctx context.Context, rec domain.SessionRecord, attempt string) (domain.SessionRecord, error) {
	s.attempt = attempt
	if s.bindErr != nil {
		return domain.SessionRecord{}, s.bindErr
	}
	return s.CreateSession(ctx, rec)
}

func TestTaskWorkerSpawnBindsBeforeRuntime(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "binding failure"}[fail], func(t *testing.T) {
			m, st, rt, _ := newManager()
			bound := &bindingWorkerStore{fakeStore: st}
			if fail {
				bound.bindErr = errors.New("binding refused")
			}
			m.store = bound
			_, _, _, err := m.Spawn(ctx, ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, Prompt: "work", TaskAttemptID: "attempt-1"})
			if bound.attempt != "attempt-1" {
				t.Fatal("attempt identity not bound")
			}
			if fail {
				if !errors.Is(err, bound.bindErr) || rt.created != 0 || len(st.sessions) != 0 {
					t.Fatalf("failed binding had side effects: err=%v runtime=%d sessions=%d", err, rt.created, len(st.sessions))
				}
			} else if err != nil || rt.created != 1 {
				t.Fatalf("spawn: %v", err)
			}
		})
	}
}

func TestTaskWorkerAlivePreservesUncertainty(t *testing.T) {
	m, st, rt, _ := newManager()
	sentinel := errors.New("probe unknown")
	rec := domain.SessionRecord{ID: "mer-1", Kind: domain.KindWorker, Metadata: domain.SessionMetadata{WorkspacePath: "/work", RuntimeHandleID: "h1"}}
	for _, tc := range []struct {
		name     string
		mutate   func(*domain.SessionRecord)
		probeErr error
		alive    bool
		wantErr  bool
	}{
		{name: "alive", alive: true}, {name: "known absent"}, {name: "probe error", probeErr: sentinel, wantErr: true},
		{name: "seed", mutate: func(r *domain.SessionRecord) { r.Metadata.WorkspacePath = "" }, wantErr: true},
		{name: "missing handle", mutate: func(r *domain.SessionRecord) { r.Metadata.RuntimeHandleID = "" }, wantErr: true},
		{name: "terminated", mutate: func(r *domain.SessionRecord) { r.IsTerminated = true }, wantErr: true},
		{name: "Chat missing", mutate: func(r *domain.SessionRecord) { r.Mode = domain.SessionModeChat }, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := rec
			if tc.mutate != nil {
				tc.mutate(&r)
			}
			st.sessions[r.ID] = r
			rt.aliveErr = tc.probeErr
			rt.aliveByHandle = map[string]bool{"h1": tc.alive}
			alive, err := m.TaskWorkerAlive(ctx, r.ID)
			if (err != nil) != tc.wantErr || alive != tc.alive {
				t.Fatalf("alive=%v err=%v", alive, err)
			}
			if tc.probeErr != nil && !errors.Is(err, sentinel) {
				t.Fatalf("lost probe error: %v", err)
			}
		})
	}
	if _, err := m.TaskWorkerAlive(ctx, "missing"); err == nil {
		t.Fatal("missing session treated as absence")
	}
	if rt.created != 0 {
		t.Fatal("observation created runtime")
	}
}

func TestTaskWorkerAliveUsesLiveChatController(t *testing.T) {
	launcher := &recordingLauncher{live: true}
	m, st, rt := newChatManager(launcher)
	st.sessions["mer-1"] = domain.SessionRecord{ID: "mer-1", Kind: domain.KindWorker, Mode: domain.SessionModeChat, Metadata: domain.SessionMetadata{WorkspacePath: "/work"}}
	alive, err := m.TaskWorkerAlive(ctx, "mer-1")
	if err != nil || !alive || rt.created != 0 {
		t.Fatalf("Chat observation: alive=%v err=%v runtime=%d", alive, err, rt.created)
	}
	launcher.live = false
	if _, err := m.TaskWorkerAlive(ctx, "mer-1"); err == nil {
		t.Fatal("missing live controller treated as absent launch")
	}
}

func TestTaskWorkerSpawnRequiresBindingStore(t *testing.T) {
	m, st, rt, _ := newManager()
	_, _, _, err := m.Spawn(ctx, ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindWorker, TaskAttemptID: "attempt", Prompt: "work"})
	if err == nil || rt.created != 0 || len(st.sessions) != 0 {
		t.Fatalf("missing binding store: err=%v runtime=%d sessions=%d", err, rt.created, len(st.sessions))
	}
}

type recoveringTaskWorkerStore struct {
	*fakeStore
	lookupErr error
}

func (s *recoveringTaskWorkerStore) IsTaskWorkerSession(context.Context, domain.SessionID) (bool, error) {
	return true, s.lookupErr
}

func TestTaskWorkerStartupNeverRelaunches(t *testing.T) {
	for _, tc := range []string{"missing handle", "unknown runtime", "dead runtime", "Chat absent", "ownership unknown", "live"} {
		t.Run(tc, func(t *testing.T) {
			launcher := &recordingLauncher{}
			m, st, rt := newChatManager(launcher)
			bound := &recoveringTaskWorkerStore{fakeStore: st}
			m.store = bound
			rec := domain.SessionRecord{ID: "mer-1", ProjectID: "mer", Kind: domain.KindWorker, Mode: domain.SessionModeTUI, Metadata: domain.SessionMetadata{WorkspacePath: "/work", Branch: "task", RuntimeHandleID: "h1"}}
			switch tc {
			case "missing handle":
				rec.Metadata.RuntimeHandleID = ""
			case "unknown runtime":
				rt.aliveErr = errors.New("unknown probe")
			case "Chat absent":
				rec.Mode = domain.SessionModeChat
			case "ownership unknown":
				bound.lookupErr = errors.New("lookup failed")
			case "live":
				rt.aliveByHandle = map[string]bool{"h1": true}
			}
			st.sessions[rec.ID] = rec
			err := m.reconcileLive(ctx, rec)
			if (err == nil) != (tc == "live") {
				t.Fatalf("recovery err=%v", err)
			}
			if rt.created != 0 || len(launcher.started) != 0 {
				t.Fatalf("recovery relaunched runtime=%d Chat=%d", rt.created, len(launcher.started))
			}
		})
	}
}

func TestTaskWorkerShutdownSavedSessionNeverRestoresAutomatically(t *testing.T) {
	m, st, rt, ws := newLifecycleManager()
	m.store = &recoveringTaskWorkerStore{fakeStore: st}
	st.sessions["mer-1"] = domain.SessionRecord{ID: "mer-1", ProjectID: "mer", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, IsTerminated: true, Activity: domain.Activity{State: domain.ActivityExited}, Metadata: domain.SessionMetadata{WorkspacePath: "/work", Branch: "task", AgentSessionID: "native"}}
	st.worktrees["mer-1"] = []domain.SessionWorktreeRecord{{SessionID: "mer-1", RepoName: domain.RootWorkspaceRepoName, State: "removed"}}
	if err := m.RestoreAll(ctx); err != nil {
		t.Fatal(err)
	}
	if rt.created != 0 || len(ws.restoreConfigs) != 0 {
		t.Fatalf("task worker auto-restored runtime=%d workspace=%d", rt.created, len(ws.restoreConfigs))
	}
	if !st.sessions["mer-1"].IsTerminated || len(st.worktrees["mer-1"]) != 1 {
		t.Fatal("held launch state modified")
	}
}
