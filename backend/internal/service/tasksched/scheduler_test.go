package tasksched

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func TestDependentTaskUnlocksWhileWorkerStaysAlive(t *testing.T) {
	s, launcher, project := newScheduler(t, domain.ScheduleLimits{PerProject: 2, PerHarness: 2})
	ctx := context.Background()
	plan := domain.TaskPlan{
		ID: "plan", ProjectID: project, Title: "Ship",
		Tasks: []domain.PlannedTask{
			{ID: "a", Title: "A", Prompt: "do a", WorkspaceKey: "ws-a", Harness: string(domain.HarnessCodex), VerificationCommands: []string{"true"}},
			{ID: "b", Title: "B", Prompt: "do b", WorkspaceKey: "ws-b", Harness: string(domain.HarnessCodex), DependsOn: []string{"a"}, VerificationCommands: []string{"true"}},
		},
	}
	if _, err := s.store.CreateTaskPlan(ctx, plan, time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	if err := s.Recover(ctx); err != nil {
		t.Fatalf("recover: %v", err)
	}
	first, err := s.Dispatch(ctx, domain.ProjectID(project), plan.ID)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(first.Claims) != 1 || first.Claims[0].TaskID != "a" {
		t.Fatalf("first claim = %+v, want only a", first.Claims)
	}
	attemptA := first.Claims[0].ID
	if !launcher.Alive(attemptA) {
		t.Fatal("worker for a should be alive after dispatch")
	}

	for _, signal := range []string{SignalProcessExit, SignalTurnCompleted} {
		_, err := s.SubmitCandidate(ctx, domain.ProjectID(project), plan.ID, "a", Candidate{AttemptID: attemptA, Signal: signal})
		var api *apierr.Error
		if err == nil || !asAPI(err, &api) || api.Code != "TASK_SIGNAL_IGNORED" {
			t.Fatalf("signal %s error = %v, want TASK_SIGNAL_IGNORED", signal, err)
		}
	}
	snap, err := s.Snapshot(ctx, domain.ProjectID(project), plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.ReadyTaskIDs) != 0 {
		t.Fatalf("b became ready without a verified result: %+v", snap.ReadyTaskIDs)
	}

	done, err := s.SubmitCandidate(ctx, domain.ProjectID(project), plan.ID, "a", Candidate{AttemptID: attemptA, Signal: SignalExplicitResult})
	if err != nil {
		t.Fatalf("explicit result: %v", err)
	}
	if done.Outcome != domain.TaskResultVerified || done.TaskState != domain.TaskStateCompleted {
		t.Fatalf("completion = %+v", done)
	}
	if !launcher.Alive(attemptA) {
		t.Fatal("verification must not require the worker to exit")
	}
	second, err := s.Dispatch(ctx, domain.ProjectID(project), plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Claims) != 1 || second.Claims[0].TaskID != "b" {
		t.Fatalf("second claim = %+v, want b", second.Claims)
	}
	if launcher.DispatchCount(attemptA) != 1 {
		t.Fatalf("task a was dispatched %d times", launcher.DispatchCount(attemptA))
	}
}

func TestRestartAtLaunchBoundaryDoesNotDuplicateWork(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.MustOpen(t)
	project := "proj"
	seedProject(t, db, project)
	now := time.Date(2026, 9, 21, 1, 0, 0, 0, time.UTC)
	plan := domain.TaskPlan{
		ID: "plan", ProjectID: project, Title: "Ship",
		Tasks: []domain.PlannedTask{{ID: "a", Title: "A", Prompt: "do a", VerificationCommands: []string{"true"}}},
	}
	if _, err := db.CreateTaskPlan(ctx, plan, now); err != nil {
		t.Fatal(err)
	}
	claimed, err := db.ClaimReadyTask(ctx, domain.ProjectID(project), plan.ID, "a", "attempt-a", domain.ScheduleLimits{}, now)
	if err != nil || claimed.Reason != "" {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	leased, err := db.LeaseAttemptDispatch(ctx, "attempt-a", now)
	if err != nil || !leased {
		t.Fatalf("lease = %v, %v", leased, err)
	}

	t.Run("ambiguous probe holds the same attempt", func(t *testing.T) {
		sched := schedulerOn(t, db, NewMemoryLauncher())
		if err := sched.Recover(ctx); err != nil {
			t.Fatal(err)
		}
		attempts, err := db.ListTaskAttempts(ctx, plan.ID, "a")
		if err != nil {
			t.Fatal(err)
		}
		if len(attempts) != 1 || attempts[0].State != domain.TaskAttemptStateBlocked {
			t.Fatalf("attempts = %+v, want one blocked attempt", attempts)
		}
		report, err := sched.Dispatch(ctx, domain.ProjectID(project), plan.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(report.Claims) != 0 {
			t.Fatalf("dispatch replaced a held attempt: %+v", report.Claims)
		}
		again, err := db.ListTaskAttempts(ctx, plan.ID, "a")
		if err != nil || len(again) != 1 {
			t.Fatalf("attempts after dispatch = %+v, %v", again, err)
		}
	})
}

func TestRestartAdoptsRecordedLaunchWithoutASecondAttempt(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.MustOpen(t)
	project := "proj"
	seedProject(t, db, project)
	now := time.Date(2026, 9, 21, 1, 0, 0, 0, time.UTC)
	plan := domain.TaskPlan{
		ID: "plan", ProjectID: project, Title: "Ship",
		Tasks: []domain.PlannedTask{{ID: "a", Title: "A", Prompt: "do a", VerificationCommands: []string{"true"}}},
	}
	if _, err := db.CreateTaskPlan(ctx, plan, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimReadyTask(ctx, domain.ProjectID(project), plan.ID, "a", "attempt-a", domain.ScheduleLimits{}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.LeaseAttemptDispatch(ctx, "attempt-a", now); err != nil {
		t.Fatal(err)
	}
	launcher := NewMemoryLauncher()
	launcher.Remember("attempt-a", "attempt:attempt-a")
	sched := schedulerOn(t, db, launcher)
	if err := sched.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	attempts, err := db.ListTaskAttempts(ctx, plan.ID, "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].State != domain.TaskAttemptStateRunning || attempts[0].RuntimeRef != "attempt:attempt-a" {
		t.Fatalf("adopted attempt = %+v", attempts)
	}
	if launcher.DispatchCount("attempt-a") != 0 {
		t.Fatal("adopting a recorded launch dispatched a second worker")
	}
}

func TestVerifiedResultRestoresDependentReadinessAfterRestart(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.MustOpen(t)
	project := "proj"
	seedProject(t, db, project)
	now := time.Date(2026, 9, 21, 2, 0, 0, 0, time.UTC)
	plan := domain.TaskPlan{
		ID: "plan", ProjectID: project, Title: "Ship",
		Tasks: []domain.PlannedTask{
			{ID: "a", Title: "A", Prompt: "do a", WorkspaceKey: "ws-a", VerificationCommands: []string{"true"}},
			{ID: "b", Title: "B", Prompt: "do b", WorkspaceKey: "ws-b", DependsOn: []string{"a"}, VerificationCommands: []string{"true"}},
		},
	}
	if _, err := db.CreateTaskPlan(ctx, plan, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimReadyTask(ctx, domain.ProjectID(project), plan.ID, "a", "attempt-a", domain.ScheduleLimits{}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.TransitionTask(ctx, plan.ID, "a", domain.TaskStateClaimed, domain.TaskStateRunning, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.TransitionTaskAttempt(ctx, "attempt-a", domain.TaskAttemptStateClaimed, domain.TaskAttemptStateRunning, nil, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.BeginCollection(ctx, plan.ID, "a", "attempt-a", now); err != nil {
		t.Fatal(err)
	}
	// Crash after the attempt is collecting and before a result exists.
	// Recovery re-runs verification from SQLite and must not claim b itself.
	sched := schedulerOn(t, db, NewMemoryLauncher())
	if err := sched.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	attempts, err := db.ListTaskAttempts(ctx, plan.ID, "b")
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 0 {
		t.Fatalf("recovery claimed the dependent: %+v", attempts)
	}
	view, ok, err := db.LoadTaskSchedule(ctx, domain.ProjectID(project), plan.ID)
	if err != nil || !ok {
		t.Fatalf("load = %v, %v", ok, err)
	}
	ready := domain.DerivedReady(view.Nodes, view.Verified)
	if len(ready) != 1 || ready[0] != "b" {
		t.Fatalf("ready after recovery = %v", ready)
	}
	report, err := sched.Dispatch(ctx, domain.ProjectID(project), plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Claims) != 1 || report.Claims[0].TaskID != "b" {
		t.Fatalf("dispatch after recovery = %+v", report.Claims)
	}
}

func TestWorkspaceAndHarnessLimits(t *testing.T) {
	s, _, project := newScheduler(t, domain.ScheduleLimits{PerProject: 2, PerHarness: 1})
	ctx := context.Background()
	plan := domain.TaskPlan{
		ID: "plan", ProjectID: project, Title: "Ship",
		Tasks: []domain.PlannedTask{
			{ID: "a", Title: "A", Prompt: "a", WorkspaceKey: "shared", Harness: string(domain.HarnessCodex)},
			{ID: "b", Title: "B", Prompt: "b", WorkspaceKey: "shared", Harness: string(domain.HarnessGrok)},
			{ID: "c", Title: "C", Prompt: "c", WorkspaceKey: "other", Harness: string(domain.HarnessCodex)},
		},
	}
	if _, err := s.store.CreateTaskPlan(ctx, plan, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	report, err := s.Dispatch(ctx, domain.ProjectID(project), plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Claims) != 1 || report.Claims[0].TaskID != "a" {
		t.Fatalf("claims = %+v, want only the first isolated-enough task", report.Claims)
	}
}

func TestDispatchBeforeRecoveryIsRefused(t *testing.T) {
	s, _, project := newScheduler(t, domain.ScheduleLimits{})
	_, err := s.Dispatch(context.Background(), domain.ProjectID(project), "missing")
	var api *apierr.Error
	if err == nil || !asAPI(err, &api) || api.Code != "TASK_RECOVERY_PENDING" {
		t.Fatalf("error = %v, want TASK_RECOVERY_PENDING", err)
	}
	if s.Ready() {
		t.Fatal("scheduler reported ready before recovery")
	}
}

type schedulerFixture struct {
	*Scheduler
	store *store.Store
}

func newScheduler(t *testing.T, limits domain.ScheduleLimits) (*schedulerFixture, *MemoryLauncher, string) {
	t.Helper()
	db := sqlitetest.MustOpen(t)
	project := "proj"
	seedProject(t, db, project)
	launcher := NewMemoryLauncher()
	sched := schedulerOn(t, db, launcher)
	sched.limits = limits.Normalized()
	return &schedulerFixture{Scheduler: sched, store: db}, launcher, project
}

func schedulerOn(t *testing.T, db *store.Store, launcher *MemoryLauncher) *Scheduler {
	t.Helper()
	var n int
	return NewWithDeps(Deps{
		Store: db, Launcher: launcher, Verifier: ExecVerifier{},
		Clock: func() time.Time { return time.Date(2026, 9, 21, 3, 0, 0, 0, time.UTC) },
		NewID: func() string {
			n++
			return fmt.Sprintf("id-%d", n)
		},
	})
}

func seedProject(t *testing.T, db *store.Store, id string) {
	t.Helper()
	if err := db.UpsertProject(context.Background(), domain.ProjectRecord{
		ID: id, Path: t.TempDir(), RegisteredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
}

func asAPI(err error, target **apierr.Error) bool {
	return errors.As(err, target)
}
