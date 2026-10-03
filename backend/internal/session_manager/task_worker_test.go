package sessionmanager

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/tasksched"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"

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

func TestTaskWorkerChatStartupReconnectsOnlyExistingCodexHost(t *testing.T) {
	for _, tc := range []string{"live host", "missing host", "missing provider", "missing generation", "missing workspace", "other harness", "terminated"} {
		t.Run(tc, func(t *testing.T) {
			launcher := &recordingLauncher{}
			launcher.afterReady = func() { launcher.live = true }
			m, st, rt := newChatManager(launcher)
			m.store = &recoveringTaskWorkerStore{fakeStore: st}
			rec := domain.SessionRecord{ID: "mer-1", ProjectID: "mer", Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat,
				Metadata: domain.SessionMetadata{WorkspacePath: t.TempDir(), ProviderConversationID: "thread-1", ControllerGeneration: "old-gen", Prompt: "never replay"}}
			switch tc {
			case "missing host":
				launcher.startErr = ports.ErrChatRecoveryInconclusive
			case "missing provider":
				rec.Metadata.ProviderConversationID = ""
			case "missing generation":
				rec.Metadata.ControllerGeneration = ""
			case "missing workspace":
				rec.Metadata.WorkspacePath = ""
			case "other harness":
				rec.Harness = domain.HarnessClaudeCode
			case "terminated":
				rec.IsTerminated = true
			}
			st.sessions[rec.ID] = rec
			err := m.reconcileLive(ctx, rec)
			if (err == nil) != (tc == "live host") {
				t.Fatalf("reconcile: %v", err)
			}
			wantStarts := 0
			if tc == "live host" || tc == "missing host" {
				wantStarts = 1
			}
			if len(launcher.started) != wantStarts {
				t.Fatalf("controller starts=%d", len(launcher.started))
			}
			for _, cfg := range launcher.started {
				if !cfg.ReconnectOnly || cfg.SessionID != rec.ID || cfg.ProviderConversationID != "thread-1" {
					t.Fatalf("unsafe recovery: %+v", cfg)
				}
			}
			if rt.created != 0 || len(launcher.turns) != 0 || len(launcher.relayed) != 0 || len(launcher.stopped) != 0 {
				t.Fatal("recovery launched work or terminated host")
			}
			recovered := st.sessions[rec.ID]
			if recovered.ID != rec.ID || recovered.Metadata.ProviderConversationID != rec.Metadata.ProviderConversationID || recovered.Metadata.WorkspacePath != rec.Metadata.WorkspacePath {
				t.Fatal("recovery changed worker identity")
			}
			if tc != "live host" && recovered.Metadata.ControllerGeneration != rec.Metadata.ControllerGeneration {
				t.Fatal("failed recovery changed ownership")
			}
			if tc == "live host" {
				if err := m.reconcileLive(ctx, recovered); err != nil {
					t.Fatal(err)
				}
				if len(launcher.started) != 1 {
					t.Fatal("repeated recovery duplicated controller")
				}
			}
		})
	}
}

type recoveredWorkerSessions struct {
	manager *Manager
	dir     string
	spawns  int
}

func (s *recoveredWorkerSessions) Spawn(ctx context.Context, cfg ports.SpawnConfig) (domain.Session, int, int, error) {
	s.spawns++
	rec, input, output, err := s.manager.Spawn(ctx, cfg)
	return domain.Session{SessionRecord: rec}, input, output, err
}
func (s *recoveredWorkerSessions) WorkspaceLocation(context.Context, domain.SessionID) (string, error) {
	return s.dir, nil
}

func TestTaskChatCrashRecoveryPreservesDurableAttemptAndSession(t *testing.T) {
	for _, survives := range []bool{true, false} {
		t.Run(map[bool]string{true: "host survives", false: "host unknown"}[survives], func(t *testing.T) {
			db := sqlitetest.MustOpen(t)
			now := time.Now()
			if err := db.UpsertProject(ctx, domain.ProjectRecord{ID: "mer", Path: t.TempDir(), Config: testRoleAgents(), RegisteredAt: now}); err != nil {
				t.Fatal(err)
			}
			plan := domain.TaskPlan{ID: "plan", ProjectID: "mer", Title: "Crash recovery", Tasks: []domain.PlannedTask{
				{ID: "a", Title: "Worker", Prompt: "do not replay", Harness: string(domain.HarnessCodex), VerificationCommands: []string{"true"}},
				{ID: "b", Title: "Dependent", Prompt: "do not dispatch", DependsOn: []string{"a"}},
			}}
			if _, err := db.CreateTaskPlan(ctx, plan, now); err != nil {
				t.Fatal(err)
			}
			claim, err := db.ClaimReadyTask(ctx, "mer", "plan", "a", "attempt", domain.ScheduleLimits{}, now)
			if err != nil || claim.Reason != "" {
				t.Fatalf("claim: %+v %v", claim, err)
			}
			if ok, err := db.LeaseAttemptDispatch(ctx, "attempt", now); err != nil || !ok {
				t.Fatalf("lease: %v %v", ok, err)
			}
			dir := t.TempDir()
			rec, err := db.CreateTaskWorkerSession(ctx, domain.SessionRecord{ProjectID: "mer", Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat,
				Metadata: domain.SessionMetadata{WorkspacePath: dir, ProviderConversationID: "thread-1", ControllerGeneration: "before-crash", Prompt: "do not replay"}, CreatedAt: now, UpdatedAt: now}, "attempt")
			if err != nil {
				t.Fatal(err)
			}
			// Crash after the worker accepted work, before recording launch completion.
			launcher := &recordingLauncher{}
			launcher.afterReady = func() { launcher.live = true }
			if !survives {
				launcher.startErr = ports.ErrChatRecoveryInconclusive
			}
			m, _, rt := newChatManager(launcher)
			m.store = db
			m.lcm = lifecycle.New(db, nil)
			m.dataDir = t.TempDir()
			recoveryErr := m.ReconcileBackground(ctx)
			if survives && recoveryErr != nil {
				t.Fatal(recoveryErr)
			}
			sessions := &recoveredWorkerSessions{manager: m, dir: dir}
			scheduler := tasksched.New(db, tasksched.NewSessionLauncher(db, sessions, m), recoveryVerifier{})
			for i := 0; i < 3; i++ {
				if err := scheduler.Recover(ctx); err != nil {
					t.Fatal(err)
				}
			}
			attempts, err := db.ListTaskAttempts(ctx, "plan", "a")
			if err != nil || len(attempts) != 1 {
				t.Fatalf("attempts=%+v err=%v", attempts, err)
			}
			want := domain.TaskAttemptStateRunning
			if !survives {
				want = domain.TaskAttemptStateBlocked
			}
			if attempts[0].ID != "attempt" || attempts[0].SessionID != string(rec.ID) || attempts[0].State != want {
				t.Fatalf("attempt changed: %+v", attempts[0])
			}
			all, err := db.ListAllSessions(ctx)
			if err != nil || len(all) != 1 || all[0].ID != rec.ID || all[0].Metadata.ProviderConversationID != "thread-1" {
				t.Fatalf("sessions=%+v err=%v", all, err)
			}
			if sessions.spawns != 0 || rt.created != 0 || len(launcher.turns) != 0 || len(launcher.relayed) != 0 || len(launcher.started) != 1 {
				t.Fatal("recovery dispatched or duplicated work")
			}
			dependent, err := db.ListTaskAttempts(ctx, "plan", "b")
			if err != nil || len(dependent) != 0 {
				t.Fatalf("dependent dispatched: %+v %v", dependent, err)
			}
		})
	}
}

type recoveryVerifier struct{}

func (recoveryVerifier) Verify(context.Context, string, []string) (tasksched.VerifyReport, error) {
	return tasksched.VerifyReport{Inconclusive: true}, nil
}
