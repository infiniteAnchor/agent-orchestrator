package tasksched

import (
	"context"
	"encoding/json"
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
	runRecoveryTicks(t, sched)
	assertAttemptCount(t, db, plan.ID, "b", 0)
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

func schedulerOn(t *testing.T, db *store.Store, launcher Launcher) *Scheduler {
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

// observeScheduleStore lets tests wait for real scheduler ticks without relying
// on a sleep or starting a provider worker.
type observeScheduleStore struct {
	Store
	ticks chan struct{}
}

func (s observeScheduleStore) ListSchedulePlans(ctx context.Context) ([]domain.TaskPlanSummary, error) {
	plans, err := s.Store.ListSchedulePlans(ctx)
	select {
	case s.ticks <- struct{}{}:
	default:
	}
	return plans, err
}

func TestAcceptanceTicksAndRestartRequireExplicitDispatch(t *testing.T) {
	s, launcher, project := newScheduler(t, domain.ScheduleLimits{})
	ctx := context.Background()
	now := time.Now().UTC()
	plan := domain.TaskPlan{ID: "accepted-plan", ProjectID: project, Title: "Ship", Tasks: []domain.PlannedTask{
		{ID: "a", Title: "A", Prompt: "do a", WorkspaceKey: "a"},
		{ID: "b", Title: "B", Prompt: "do b", WorkspaceKey: "b"},
	}}
	proposal, _, err := s.store.CreateTaskPlanProposal(ctx, domain.TaskPlanProposal{
		ID: "proposal", ProjectID: domain.ProjectID(project), RequestKey: "proposal-request", Specification: "Ship", CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	graph, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	proposal.GraphJSON = string(graph)
	proposal.State = domain.TaskPlanProposalReady
	if applied, err := s.store.SetTaskPlanProposalState(ctx, proposal, domain.TaskPlanProposalQueued, now); err != nil || !applied {
		t.Fatalf("ready proposal = %v, %v", applied, err)
	}
	if _, err := s.store.AcceptTaskPlanProposal(ctx, domain.ProjectID(project), proposal.ID, plan, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.store.OpenHumanGate(ctx, domain.HumanGate{
		ID: "gate", ProjectID: domain.ProjectID(project), PlanID: plan.ID, TaskID: "b", RequestKey: "gate-request", Summary: "Review B", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// Startup and idle ticks see the accepted plan but cannot claim even a,
	// which has no gate. Approval likewise does not authorize work.
	runRecoveryTicks(t, s.Scheduler)
	assertAttemptCount(t, s.store, plan.ID, "a", 0)
	assertAttemptCount(t, s.store, plan.ID, "b", 0)
	first, err := s.Dispatch(ctx, domain.ProjectID(project), plan.ID)
	if err != nil || len(first.Claims) != 1 || first.Claims[0].TaskID != "a" {
		t.Fatalf("explicit dispatch with task-scoped gate = %+v, %v", first, err)
	}
	if _, err := s.store.ResolveHumanGate(ctx, domain.ProjectID(project), "gate", domain.HumanGateApproved, now); err != nil {
		t.Fatal(err)
	}
	// A fresh scheduler models restart. It preserves a's authorized launch,
	// while b remains inert even after approval and repeated recovery ticks.
	restarted := schedulerOn(t, s.store, launcher)
	restarted.newID = func() string { return "restart-attempt" }
	runRecoveryTicks(t, restarted)
	assertAttemptCount(t, s.store, plan.ID, "a", 1)
	assertAttemptCount(t, s.store, plan.ID, "b", 0)
	second, err := restarted.Dispatch(ctx, domain.ProjectID(project), plan.ID)
	if err != nil || len(second.Claims) != 1 || second.Claims[0].TaskID != "b" {
		t.Fatalf("explicit dispatch after approval = %+v, %v", second, err)
	}
	again, err := restarted.Dispatch(ctx, domain.ProjectID(project), plan.ID)
	if err != nil || len(again.Claims) != 0 {
		t.Fatalf("repeated dispatch = %+v, %v", again, err)
	}
	for _, attempt := range append(first.Claims, second.Claims...) {
		if got := launcher.DispatchCount(attempt.ID); got != 1 {
			t.Fatalf("attempt %s dispatched %d times", attempt.ID, got)
		}
		assertAttemptCount(t, s.store, plan.ID, attempt.TaskID, 1)
	}
}

func runRecoveryTicks(t *testing.T, s *Scheduler) {
	t.Helper()
	original := s.store
	observed := observeScheduleStore{Store: original, ticks: make(chan struct{}, 8)}
	s.store = observed
	s.interval = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("scheduler Run: %v", err)
		}
		s.store = original
	}()
	for i := 0; i < 3; i++ {
		select {
		case <-observed.ticks:
		case <-time.After(5 * time.Second):
			t.Fatal("scheduler did not recover and tick")
		}
	}
}

func assertAttemptCount(t *testing.T, db *store.Store, planID, taskID string, want int) {
	t.Helper()
	attempts, err := db.ListTaskAttempts(context.Background(), planID, taskID)
	if err != nil || len(attempts) != want {
		t.Fatalf("task %s attempts = %+v, %v; want %d", taskID, attempts, err, want)
	}
}

func TestRecordedLaunchUnknownRuntimeIsHeldWithoutReplacement(t *testing.T) {
	s, launcher, project := newScheduler(t, domain.ScheduleLimits{})
	ctx := context.Background()
	plan := domain.TaskPlan{ID: "plan", ProjectID: project, Title: "Ship", Tasks: []domain.PlannedTask{{ID: "a", Title: "A", Prompt: "a"}}}
	if _, err := s.store.CreateTaskPlan(ctx, plan, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	dispatched, err := s.Dispatch(ctx, domain.ProjectID(project), plan.ID)
	if err != nil || len(dispatched.Claims) != 1 {
		t.Fatalf("dispatch = %+v, %v", dispatched, err)
	}
	attemptID := dispatched.Claims[0].ID
	launcher.Override(attemptID, LaunchAmbiguous)
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	view, _, err := s.store.LoadTaskSchedule(ctx, domain.ProjectID(project), plan.ID)
	if err != nil || len(view.Attempts) != 1 || view.Attempts[0].State != domain.TaskAttemptStateBlocked {
		t.Fatalf("unknown runtime attempt = %+v, %v", view.Attempts, err)
	}
	if report, err := s.Dispatch(ctx, domain.ProjectID(project), plan.ID); err != nil || len(report.Claims) != 0 {
		t.Fatalf("dispatch replaced unknown runtime = %+v, %v", report, err)
	}
	if launcher.DispatchCount(attemptID) != 1 {
		t.Fatal("unknown runtime launched a replacement")
	}
}

type workspaceTestLauncher struct {
	*MemoryLauncher
	location  string
	sessionID string
	err       error
}

func (l workspaceTestLauncher) Dispatch(ctx context.Context, req LaunchRequest) (LaunchOutcome, error) {
	outcome, err := l.MemoryLauncher.Dispatch(ctx, req)
	outcome.SessionID = l.sessionID
	return outcome, err
}

func (l workspaceTestLauncher) WorkspaceLocation(context.Context, domain.SessionID) (string, error) {
	return l.location, l.err
}

type directoryVerifier struct{ dir string }

func (v *directoryVerifier) Verify(_ context.Context, dir string, _ []string) (VerifyReport, error) {
	v.dir = dir
	return VerifyReport{Passed: dir != "", Inconclusive: dir == ""}, nil
}

func TestWorkerResultUsesIsolatedWorkspaceOrIsInconclusive(t *testing.T) {
	for _, state := range []string{"available", "lookup_failure", "no_resolver"} {
		t.Run(state, func(t *testing.T) {
			unavailable := state != "available"
			s, memory, project := newScheduler(t, domain.ScheduleLimits{})
			ctx := context.Background()
			plan := domain.TaskPlan{ID: "plan", ProjectID: project, Title: "Ship", Tasks: []domain.PlannedTask{{ID: "a", Title: "A", Prompt: "a", VerificationCommands: []string{"true"}}}}
			if _, err := s.store.CreateTaskPlan(ctx, plan, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			worker, err := s.store.CreateSession(ctx, domain.SessionRecord{ProjectID: domain.ProjectID(project), Kind: domain.KindWorker, Mode: domain.SessionModeTUI, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
			if err != nil {
				t.Fatal(err)
			}
			launcher := workspaceTestLauncher{MemoryLauncher: memory, location: t.TempDir(), sessionID: string(worker.ID)}
			wantDir := launcher.location
			if unavailable {
				launcher.err = errors.New("workspace unavailable")
				wantDir = ""
			}
			s.launcher = launcher
			if state == "no_resolver" {
				s.launcher = struct{ Launcher }{launcher}
			}
			verifier := &directoryVerifier{}
			s.verifier = verifier
			if err := s.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			dispatched, err := s.Dispatch(ctx, domain.ProjectID(project), plan.ID)
			if err != nil || len(dispatched.Claims) != 1 {
				t.Fatalf("dispatch = %+v, %v", dispatched, err)
			}
			done, err := s.SubmitCandidate(ctx, domain.ProjectID(project), plan.ID, "a", Candidate{AttemptID: dispatched.Claims[0].ID, Signal: SignalExplicitResult})
			if err != nil {
				t.Fatal(err)
			}
			if verifier.dir != wantDir {
				t.Fatalf("verification directory = %q, want %q", verifier.dir, wantDir)
			}
			wantOutcome := domain.TaskResultVerified
			if unavailable {
				wantOutcome = domain.TaskResultInconclusive
			}
			if done.Outcome != wantOutcome {
				t.Fatalf("result = %s, want %s", done.Outcome, wantOutcome)
			}
		})
	}
}
