package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// A stored graph must read back exactly as it was declared. Phase and task
// collections carry a `position` column precisely so that declaration order
// survives the round trip: the slice-1 contract encodes order in slice position,
// and neither input order nor rowid is a safe stand-in for it.
func TestTaskGraphStore_RoundTripsGraphPreservingOrder(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "proj-1")
	now := time.Now().UTC().Truncate(time.Second)

	plan := domain.TaskPlan{
		ID:        "plan-1",
		ProjectID: "proj-1",
		Title:     "Ship it",
		// Declared out of natural order on purpose: the phase list and the task
		// list are both in "verify first" order, while the dependency runs the
		// other way.
		Phases: []domain.TaskPhase{
			{ID: "p2", Title: "Verify"},
			{ID: "p1", Title: "Build"},
		},
		Tasks: []domain.PlannedTask{
			{ID: "t-verify", PhaseID: "p2", Title: "Verify", Prompt: "verify it", DependsOn: []string{"t-build"}},
			{
				ID: "t-build", PhaseID: "p1", Title: "Build", Prompt: "build it",
				VerificationCommands: []string{"go build ./...", "go test ./..."},
			},
			{ID: "t-lint", PhaseID: "p1", Title: "Lint", Prompt: "lint it", VerificationCommands: []string{"golangci-lint run"}},
		},
	}

	summary, err := s.CreateTaskPlan(ctx, plan, now)
	if err != nil {
		t.Fatalf("create task plan: %v", err)
	}
	if summary.ID != plan.ID || summary.ProjectID != plan.ProjectID || summary.Title != plan.Title {
		t.Fatalf("summary does not describe the stored plan: %+v", summary)
	}
	if !summary.CreatedAt.Equal(now) || !summary.UpdatedAt.Equal(now) {
		t.Fatalf("summary timestamps = %v/%v, want %v", summary.CreatedAt, summary.UpdatedAt, now)
	}

	got, ok, err := s.GetTaskPlan(ctx, "proj-1", "plan-1")
	if err != nil {
		t.Fatalf("get task plan: %v", err)
	}
	if !ok {
		t.Fatal("plan was created but GetTaskPlan reports not found")
	}

	if len(got.Phases) != 2 || got.Phases[0].ID != "p2" || got.Phases[1].ID != "p1" {
		t.Fatalf("phase order not preserved: %+v", got.Phases)
	}
	if len(got.Tasks) != 3 {
		t.Fatalf("want 3 tasks, got %d", len(got.Tasks))
	}
	if got.Tasks[0].ID != "t-verify" || got.Tasks[1].ID != "t-build" || got.Tasks[2].ID != "t-lint" {
		t.Fatalf("task order not preserved: %s/%s/%s", got.Tasks[0].ID, got.Tasks[1].ID, got.Tasks[2].ID)
	}
	if got.Tasks[0].PhaseID != "p2" || got.Tasks[1].PhaseID != "p1" {
		t.Fatalf("task phase assignment lost: %q/%q", got.Tasks[0].PhaseID, got.Tasks[1].PhaseID)
	}
	if len(got.Tasks[0].DependsOn) != 1 || got.Tasks[0].DependsOn[0] != "t-build" {
		t.Fatalf("dependency not preserved: %+v", got.Tasks[0].DependsOn)
	}
	// Command order matters to the caller that will run them, so it must survive.
	wantCommands := []string{"go build ./...", "go test ./..."}
	if len(got.Tasks[1].VerificationCommands) != 2 ||
		got.Tasks[1].VerificationCommands[0] != wantCommands[0] ||
		got.Tasks[1].VerificationCommands[1] != wantCommands[1] {
		t.Fatalf("verification command order not preserved: %+v", got.Tasks[1].VerificationCommands)
	}
	// The re-read graph must still pass the same validation it passed on write.
	if err := got.Validate(); err != nil {
		t.Fatalf("round-tripped graph fails validation: %v", err)
	}

	// A fresh task starts queued. Readiness is NOT stored, so no other state is
	// invented here.
	attempts, err := s.ListTaskAttempts(ctx, "plan-1", "t-build")
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	if len(attempts) != 0 {
		t.Fatalf("creating a plan must not claim any task, got %d attempts", len(attempts))
	}
}

// Phases are optional grouping metadata; a plan without them must persist.
func TestTaskGraphStore_PhasesAreOptional(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "proj-1")

	plan := domain.TaskPlan{
		ID:        "plan-no-phases",
		ProjectID: "proj-1",
		Title:     "Solo",
		Tasks:     []domain.PlannedTask{{ID: "t1", Title: "One", Prompt: "do it"}},
	}
	if _, err := s.CreateTaskPlan(ctx, plan, time.Now()); err != nil {
		t.Fatalf("create plan without phases: %v", err)
	}
	got, ok, err := s.GetTaskPlan(ctx, "proj-1", plan.ID)
	if err != nil || !ok {
		t.Fatalf("get plan without phases: ok=%v err=%v", ok, err)
	}
	if len(got.Phases) != 0 {
		t.Fatalf("want no phases, got %+v", got.Phases)
	}
	if len(got.Tasks) != 1 || got.Tasks[0].PhaseID != "" {
		t.Fatalf("want one task with no phase, got %+v", got.Tasks)
	}
}

// Validation happens before the first row is written, so a rejected graph leaves
// no partial plan and no CDC events behind.
func TestTaskGraphStore_RejectsInvalidGraphWithoutPersisting(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "proj-1")
	now := time.Now()

	// Slice-1 validation reports the verification requirement before cycle
	// detection, so the fixture supplies commands and isolates the cycle rule.
	cyclic := domain.TaskPlan{
		ID:        "plan-cyclic",
		ProjectID: "proj-1",
		Title:     "Cycle",
		Tasks: []domain.PlannedTask{
			{ID: "a", Title: "A", Prompt: "a", DependsOn: []string{"b"}, VerificationCommands: []string{"true"}},
			{ID: "b", Title: "B", Prompt: "b", DependsOn: []string{"a"}, VerificationCommands: []string{"true"}},
		},
	}
	if _, err := s.CreateTaskPlan(ctx, cyclic, now); err == nil {
		t.Fatal("want a cycle rejection, got nil error")
	} else if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("want a cycle error, got %v", err)
	}

	if _, ok, err := s.GetTaskPlan(ctx, "proj-1", "plan-cyclic"); err != nil || ok {
		t.Fatalf("rejected plan must not be readable: ok=%v err=%v", ok, err)
	}
	// A half-written plan would have emitted a plan-created event; none may exist.
	rows, err := s.EventsAfter(ctx, 0, 100)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	for _, row := range rows {
		if strings.HasPrefix(string(row.Type), "task_") {
			t.Fatalf("rejected plan emitted %s", row.Type)
		}
	}

	// A task whose dependents would be unlocked without a verification command is
	// also rejected: readiness can never become provable for it.
	unverifiable := domain.TaskPlan{
		ID:        "plan-unverifiable",
		ProjectID: "proj-1",
		Title:     "Unverifiable",
		Tasks: []domain.PlannedTask{
			{ID: "a", Title: "A", Prompt: "a"},
			{ID: "b", Title: "B", Prompt: "b", DependsOn: []string{"a"}},
		},
	}
	if _, err := s.CreateTaskPlan(ctx, unverifiable, now); err == nil {
		t.Fatal("want a rejection for a task that unlocks dependents without verification, got nil")
	}
}

// A plan is scoped to its project: guessing a plan id must not read another
// project's graph.
func TestTaskGraphStore_ScopesPlansToProject(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "proj-1")
	seedProject(t, s, "proj-2")
	now := time.Now()

	plan := domain.TaskPlan{
		ID:        "plan-1",
		ProjectID: "proj-1",
		Title:     "Owner",
		Tasks:     []domain.PlannedTask{{ID: "t1", Title: "One", Prompt: "do it"}},
	}
	if _, err := s.CreateTaskPlan(ctx, plan, now); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, ok, err := s.GetTaskPlan(ctx, "proj-2", "plan-1"); err != nil || ok {
		t.Fatalf("another project must not read the plan: ok=%v err=%v", ok, err)
	}
	plans, err := s.ListTaskPlans(ctx, "proj-2", time.Time{}, "", 100)
	if err != nil {
		t.Fatalf("list for other project: %v", err)
	}
	if len(plans) != 0 {
		t.Fatalf("another project must list no plans, got %+v", plans)
	}

	// A plan for a project that does not exist is refused by the schema rather
	// than silently orphaned.
	orphan := plan
	orphan.ID = "plan-orphan"
	orphan.ProjectID = "proj-missing"
	if _, err := s.CreateTaskPlan(ctx, orphan, now); err == nil {
		t.Fatal("want an FK rejection for an unknown project, got nil")
	}
}

func TestTaskGraphStore_ListsPlanSummariesNewestFirst(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "proj-1")
	base := time.Now().UTC().Truncate(time.Second)

	for i, id := range []string{"plan-old", "plan-new"} {
		plan := domain.TaskPlan{
			ID:        id,
			ProjectID: "proj-1",
			Title:     id,
			Tasks:     []domain.PlannedTask{{ID: "t1", Title: "One", Prompt: "do it"}},
		}
		if _, err := s.CreateTaskPlan(ctx, plan, base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}

	plans, err := s.ListTaskPlans(ctx, "proj-1", time.Time{}, "", 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(plans) != 2 {
		t.Fatalf("want 2 plans, got %d", len(plans))
	}
	if plans[0].ID != "plan-new" || plans[1].ID != "plan-old" {
		t.Fatalf("want newest first, got %s then %s", plans[0].ID, plans[1].ID)
	}
}

func TestTaskGraphStore_PaginatesPlansWithStableTimestampTieBreak(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "proj-1")
	now := time.Now().UTC().Truncate(time.Second)
	for _, id := range []string{"plan-c", "plan-a", "plan-b"} {
		if _, err := s.CreateTaskPlan(ctx, domain.TaskPlan{
			ID: id, ProjectID: "proj-1", Title: id,
			Tasks: []domain.PlannedTask{{ID: "t1", Title: "One", Prompt: "do it"}},
		}, now); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	first, err := s.ListTaskPlans(ctx, "proj-1", time.Time{}, "", 2)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first) != 2 || first[0].ID != "plan-a" || first[1].ID != "plan-b" {
		t.Fatalf("first page = %+v", first)
	}
	second, err := s.ListTaskPlans(ctx, "proj-1", first[1].CreatedAt, first[1].ID, 2)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second) != 1 || second[0].ID != "plan-c" {
		t.Fatalf("second page = %+v", second)
	}
}

func TestTaskGraphStore_MapsDuplicatePlanIdentity(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "proj-1")
	seedProject(t, s, "proj-2")
	plan := domain.TaskPlan{
		ID: "same-id", ProjectID: "proj-1", Title: "First",
		Tasks: []domain.PlannedTask{{ID: "t1", Title: "One", Prompt: "do it"}},
	}
	if _, err := s.CreateTaskPlan(ctx, plan, time.Now()); err != nil {
		t.Fatalf("first create: %v", err)
	}
	plan.ProjectID = "proj-2"
	if _, err := s.CreateTaskPlan(ctx, plan, time.Now()); !errors.Is(err, domain.ErrDuplicateTaskPlan) {
		t.Fatalf("duplicate error = %v, want ErrDuplicateTaskPlan", err)
	}
}

func TestTaskGraphStore_RefusesMissingOrArchivedProjectAtomically(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	plan := domain.TaskPlan{
		ID: "plan", ProjectID: "missing", Title: "Plan",
		Tasks: []domain.PlannedTask{{ID: "t1", Title: "One", Prompt: "do it"}},
	}
	if _, err := s.CreateTaskPlan(ctx, plan, time.Now()); !errors.Is(err, domain.ErrTaskPlanProjectNotFound) {
		t.Fatalf("missing project error = %v", err)
	}

	seedProject(t, s, "archived")
	if ok, err := s.ArchiveProject(ctx, "archived", time.Now()); err != nil || !ok {
		t.Fatalf("archive project: ok=%v err=%v", ok, err)
	}
	plan.ProjectID = "archived"
	if _, err := s.CreateTaskPlan(ctx, plan, time.Now()); !errors.Is(err, domain.ErrTaskPlanProjectNotFound) {
		t.Fatalf("archived project error = %v", err)
	}
}

// Transitions compare-and-swap on the current state. The loser of a concurrent
// claim must observe applied=false rather than double-dispatching the task.
func TestTaskGraphStore_TaskTransitionIsCompareAndSwap(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "proj-1")
	now := time.Now()
	plan := domain.TaskPlan{
		ID:        "plan-1",
		ProjectID: "proj-1",
		Title:     "T",
		Tasks:     []domain.PlannedTask{{ID: "t1", Title: "One", Prompt: "do it"}},
	}
	if _, err := s.CreateTaskPlan(ctx, plan, now); err != nil {
		t.Fatalf("create: %v", err)
	}

	applied, err := s.TransitionTask(ctx, "plan-1", "t1", domain.TaskStateQueued, domain.TaskStateClaimed, now)
	if err != nil || !applied {
		t.Fatalf("first claim should apply: applied=%v err=%v", applied, err)
	}
	applied, err = s.TransitionTask(ctx, "plan-1", "t1", domain.TaskStateQueued, domain.TaskStateClaimed, now)
	if err != nil {
		t.Fatalf("losing claim must not error: %v", err)
	}
	if applied {
		t.Fatal("the second claim from queued must not apply: the task is already claimed")
	}

	if _, err := s.TransitionTask(ctx, "plan-1", "t1", domain.TaskStateCompleted, domain.TaskStateRunning, now); err == nil {
		t.Fatal("want an illegal-transition error for completed -> running")
	}
	if _, err := s.TransitionTask(ctx, "plan-1", "t1", domain.TaskStateClaimed, domain.TaskStateClaimed, now); err == nil {
		t.Fatal("a same-state transition must be rejected rather than emit a no-op event")
	}
}

func TestTaskGraphStore_AttemptsAreNumberedAndCompareAndSwap(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "proj-1")
	base := time.Now().UTC().Truncate(time.Second)
	seedTaskGraph(t, s, "proj-1", "plan-1", base)
	session := seedWorkerSession(t, s, "proj-1")

	first, err := s.CreateTaskAttempt(ctx, domain.TaskAttempt{
		ID: "attempt-1", PlanID: "plan-1", TaskID: "t1",
	}, base)
	if err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	if first.AttemptNumber != 1 {
		t.Fatalf("first attempt number = %d, want 1", first.AttemptNumber)
	}
	if first.State != domain.TaskAttemptStateClaimed {
		t.Fatalf("a new attempt should be claimed, got %s", first.State)
	}
	if first.SessionID != "" || first.Harness != "" {
		t.Fatalf("a claim must not invent a session or harness: %+v", first)
	}

	// A claim is persisted before the runtime is launched; the missing session is
	// the whole point, so it must not be required.
	if _, err := s.CreateTaskAttempt(ctx, domain.TaskAttempt{
		ID: "attempt-2", PlanID: "plan-1", TaskID: "t1", AttemptNumber: 2,
	}, base); err != nil {
		t.Fatalf("create second attempt: %v", err)
	}
	auto, err := s.CreateTaskAttempt(ctx, domain.TaskAttempt{
		ID: "attempt-3", PlanID: "plan-1", TaskID: "t1",
	}, base)
	if err != nil {
		t.Fatalf("create third attempt: %v", err)
	}
	if auto.AttemptNumber != 3 {
		t.Fatalf("auto-allocated attempt number = %d, want 3", auto.AttemptNumber)
	}

	updated, applied, err := s.TransitionTaskAttempt(
		ctx, "attempt-1", domain.TaskAttemptStateClaimed, domain.TaskAttemptStateRunning, &session, base.Add(time.Second))
	if err != nil || !applied {
		t.Fatalf("claim -> running: applied=%v err=%v", applied, err)
	}
	if updated.SessionID != string(session) {
		t.Fatalf("session association lost: %q", updated.SessionID)
	}
	if updated.StartedAt == nil {
		t.Fatal("entering running should record started_at")
	}

	_, applied, err = s.TransitionTaskAttempt(
		ctx, "attempt-1", domain.TaskAttemptStateClaimed, domain.TaskAttemptStateRunning, nil, base.Add(2*time.Second))
	if err != nil {
		t.Fatalf("losing transition must not error: %v", err)
	}
	if applied {
		t.Fatal("the second claim -> running must not apply: the attempt already moved")
	}

	got, ok, err := s.GetTaskAttempt(ctx, "attempt-1")
	if err != nil || !ok {
		t.Fatalf("get attempt: ok=%v err=%v", ok, err)
	}
	if got.State != domain.TaskAttemptStateRunning || got.SessionID != string(session) {
		t.Fatalf("stored attempt = %+v", got)
	}

	if _, _, err := s.TransitionTaskAttempt(
		ctx, "attempt-1", domain.TaskAttemptStateCompleted, domain.TaskAttemptStateRunning, nil, base); err == nil {
		t.Fatal("want an illegal-transition error from a terminal state")
	}
}

// A recorded result is the durable evidence a dependent task unlocks on, so it
// must settle its attempt in the same transaction and never be rewritable.
func TestTaskGraphStore_RecordsResultAndSettlesAttempt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "proj-1")
	base := time.Now().UTC().Truncate(time.Second)
	seedTaskGraph(t, s, "proj-1", "plan-1", base)

	session := seedWorkerSession(t, s, "proj-1")
	if _, err := s.CreateTaskAttempt(ctx, domain.TaskAttempt{
		ID: "attempt-1", PlanID: "plan-1", TaskID: "t1", SessionID: string(session),
	}, base); err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	if _, applied, err := s.TransitionTaskAttempt(
		ctx, "attempt-1", domain.TaskAttemptStateClaimed, domain.TaskAttemptStateRunning, &session, base); err != nil || !applied {
		t.Fatalf("claim -> running: applied=%v err=%v", applied, err)
	}
	if _, applied, err := s.TransitionTaskAttempt(
		ctx, "attempt-1", domain.TaskAttemptStateRunning, domain.TaskAttemptStateCollecting, &session, base.Add(30*time.Second)); err != nil || !applied {
		t.Fatalf("running -> collecting: applied=%v err=%v", applied, err)
	}

	// Recording straight out of running would skip result collection, so it is
	// refused: evidence is gathered during the collecting phase.
	premature := domain.TaskResult{
		ID: "result-premature", PlanID: "plan-1", TaskID: "t1", AttemptID: "attempt-1",
		Outcome: domain.TaskResultVerified, Evidence: []byte(`{"commands":[]}`), RecordedAt: base,
	}
	if _, _, err := s.RecordTaskResult(ctx, premature, domain.TaskAttemptStateRunning); err == nil {
		t.Fatal("want a rejection for recording a result without collecting first")
	}

	result := domain.TaskResult{
		ID:         "result-1",
		PlanID:     "plan-1",
		TaskID:     "t1",
		AttemptID:  "attempt-1",
		Outcome:    domain.TaskResultVerified,
		Summary:    "all commands passed",
		Evidence:   []byte(`{"commands":[{"command":"go test ./...","exitCode":0}]}`),
		RecordedAt: base.Add(time.Minute),
	}
	if _, applied, err := s.RecordTaskResult(ctx, result, domain.TaskAttemptStateCollecting); err != nil || !applied {
		t.Fatalf("record result: applied=%v err=%v", applied, err)
	}

	attempt, ok, err := s.GetTaskAttempt(ctx, "attempt-1")
	if err != nil || !ok {
		t.Fatalf("get attempt: ok=%v err=%v", ok, err)
	}
	if attempt.State != domain.TaskAttemptStateCompleted {
		t.Fatalf("a verified result should settle the attempt to completed, got %s", attempt.State)
	}
	if attempt.FinishedAt == nil {
		t.Fatal("settling an attempt should record finished_at")
	}

	stored, ok, err := s.GetTaskResultByAttempt(ctx, "attempt-1")
	if err != nil || !ok {
		t.Fatalf("get result: ok=%v err=%v", ok, err)
	}
	if stored.Outcome != domain.TaskResultVerified || stored.Summary != result.Summary {
		t.Fatalf("stored result = %+v", stored)
	}
	if string(stored.Evidence) != string(result.Evidence) {
		t.Fatalf("evidence not preserved: %s", stored.Evidence)
	}

	// The attempt is terminal, so a second result for it is refused rather than
	// overwriting the evidence that already unlocked a dependent task.
	second := result
	second.ID = "result-2"
	// The attempt already settled, so this is a stale caller view: the CAS loses
	// and no second result exists. One attempt yields one result, which is what
	// keeps "verified" unambiguous.
	if _, applied, err := s.RecordTaskResult(ctx, second, domain.TaskAttemptStateCollecting); err != nil {
		t.Fatalf("a stale second result must lose the CAS, not error: %v", err)
	} else if applied {
		t.Fatal("an attempt must not accept a second result")
	}
	if _, ok, err := s.GetTaskResultByAttempt(ctx, "attempt-1"); err != nil || !ok {
		t.Fatalf("the first result must survive: ok=%v err=%v", ok, err)
	}

	// Naming a terminal state as the source is a caller bug rather than a race:
	// no transition can ever leave a terminal state.
	terminal := result
	terminal.ID = "result-terminal"
	if _, _, err := s.RecordTaskResult(ctx, terminal, domain.TaskAttemptStateCompleted); err == nil {
		t.Fatal("want a rejection when the caller declares the attempt terminal")
	}

	// A stale named state loses the compare-and-swap instead of recording
	// evidence against an attempt that is in some other state.
	if _, err := s.CreateTaskAttempt(ctx, domain.TaskAttempt{
		ID: "attempt-2", PlanID: "plan-1", TaskID: "t1",
	}, base); err != nil {
		t.Fatalf("create attempt-2: %v", err)
	}
	third := result
	third.ID = "result-3"
	third.AttemptID = "attempt-2"
	_, applied, err := s.RecordTaskResult(ctx, third, domain.TaskAttemptStateCollecting)
	if err != nil {
		t.Fatalf("a stale state must lose the CAS, not error: %v", err)
	}
	if applied {
		t.Fatal("a stale named state must not record a result")
	}
	if _, ok, err := s.GetTaskResultByAttempt(ctx, "attempt-2"); err != nil || ok {
		t.Fatalf("no result may be recorded for attempt-2: ok=%v err=%v", ok, err)
	}
}

// A result must describe the attempt it settles. Readiness derives per task, so
// a mis-attributed result would unlock the wrong task's dependents, and the
// schema cannot catch it: the foreign keys tie the result to a task and to an
// attempt independently.
func TestTaskGraphStore_RejectsResultAttributedToAnotherTask(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "proj-1")
	base := time.Now().UTC().Truncate(time.Second)
	plan := domain.TaskPlan{
		ID:        "plan-1",
		ProjectID: "proj-1",
		Title:     "T",
		Tasks: []domain.PlannedTask{
			{ID: "t1", Title: "One", Prompt: "do one"},
			{ID: "t2", Title: "Two", Prompt: "do two"},
		},
	}
	if _, err := s.CreateTaskPlan(ctx, plan, base); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	if _, err := s.CreateTaskAttempt(ctx, domain.TaskAttempt{
		ID: "attempt-t1", PlanID: "plan-1", TaskID: "t1",
	}, base); err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	session := seedWorkerSession(t, s, "proj-1")
	if _, applied, err := s.TransitionTaskAttempt(
		ctx, "attempt-t1", domain.TaskAttemptStateClaimed, domain.TaskAttemptStateRunning, &session, base); err != nil || !applied {
		t.Fatalf("claim -> running: applied=%v err=%v", applied, err)
	}
	if _, applied, err := s.TransitionTaskAttempt(
		ctx, "attempt-t1", domain.TaskAttemptStateRunning, domain.TaskAttemptStateCollecting, &session, base); err != nil || !applied {
		t.Fatalf("running -> collecting: applied=%v err=%v", applied, err)
	}

	// t1's attempt, but the result claims t2.
	_, _, err := s.RecordTaskResult(ctx, domain.TaskResult{
		ID: "result-misattributed", PlanID: "plan-1", TaskID: "t2", AttemptID: "attempt-t1",
		Outcome: domain.TaskResultVerified, Evidence: []byte(`{"commands":[]}`), RecordedAt: base,
	}, domain.TaskAttemptStateCollecting)
	if err == nil {
		t.Fatal("want a rejection for a result attributed to a task its attempt does not belong to")
	}
	if !strings.Contains(err.Error(), "belongs to task t1") {
		t.Fatalf("error should name the real owner: %v", err)
	}

	// The whole transaction rolls back, so the rejected attempt is not left
	// settled by evidence that was never recorded.
	attempt, ok, err := s.GetTaskAttempt(ctx, "attempt-t1")
	if err != nil || !ok {
		t.Fatalf("get attempt: ok=%v err=%v", ok, err)
	}
	if attempt.State != domain.TaskAttemptStateCollecting {
		t.Fatalf("a rejected result must not settle the attempt, got %s", attempt.State)
	}
	if _, ok, err := s.GetTaskResultByAttempt(ctx, "attempt-t1"); err != nil || ok {
		t.Fatalf("no result may be recorded: ok=%v err=%v", ok, err)
	}
	verified, err := s.ListVerifiedTaskResults(ctx, "plan-1")
	if err != nil {
		t.Fatalf("list verified: %v", err)
	}
	if len(verified) != 0 {
		t.Fatalf("a rejected result must not become unlock evidence, got %+v", verified)
	}
}

// An inconclusive result is held, not read as success: only outcome='verified'
// may unlock dependent work.
func TestTaskGraphStore_ListsOnlyVerifiedResultsAsUnlockEvidence(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "proj-1")
	base := time.Now().UTC().Truncate(time.Second)
	seedTaskGraph(t, s, "proj-1", "plan-1", base)

	outcomes := []struct {
		attemptID string
		outcome   domain.TaskResultOutcome
		from      domain.TaskAttemptState
	}{
		{"attempt-verified", domain.TaskResultVerified, domain.TaskAttemptStateCollecting},
		{"attempt-failed", domain.TaskResultFailed, domain.TaskAttemptStateCollecting},
		{"attempt-inconclusive", domain.TaskResultInconclusive, domain.TaskAttemptStateCollecting},
	}
	for _, o := range outcomes {
		if _, err := s.CreateTaskAttempt(ctx, domain.TaskAttempt{
			ID: o.attemptID, PlanID: "plan-1", TaskID: "t1",
		}, base); err != nil {
			t.Fatalf("create %s: %v", o.attemptID, err)
		}
		session := seedWorkerSession(t, s, "proj-1")
		if _, applied, err := s.TransitionTaskAttempt(
			ctx, o.attemptID, domain.TaskAttemptStateClaimed, domain.TaskAttemptStateRunning, &session, base); err != nil || !applied {
			t.Fatalf("claim -> running for %s: applied=%v err=%v", o.attemptID, applied, err)
		}
		if _, applied, err := s.TransitionTaskAttempt(
			ctx, o.attemptID, domain.TaskAttemptStateRunning, domain.TaskAttemptStateCollecting, &session, base); err != nil || !applied {
			t.Fatalf("running -> collecting for %s: applied=%v err=%v", o.attemptID, applied, err)
		}
		if _, applied, err := s.RecordTaskResult(ctx, domain.TaskResult{
			ID:         "result-" + o.attemptID,
			PlanID:     "plan-1",
			TaskID:     "t1",
			AttemptID:  o.attemptID,
			Outcome:    o.outcome,
			Evidence:   []byte(`{"commands":[]}`),
			RecordedAt: base.Add(time.Minute),
		}, o.from); err != nil || !applied {
			t.Fatalf("record %s: applied=%v err=%v", o.outcome, applied, err)
		}
	}

	all, err := s.ListTaskResults(ctx, "plan-1")
	if err != nil {
		t.Fatalf("list results: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("want 3 recorded results, got %d", len(all))
	}

	verified, err := s.ListVerifiedTaskResults(ctx, "plan-1")
	if err != nil {
		t.Fatalf("list verified results: %v", err)
	}
	if len(verified) != 1 || verified[0].ID != "result-attempt-verified" {
		t.Fatalf("want only the verified result, got %+v", verified)
	}

	// The non-verified outcomes settle their attempts differently: a failed result
	// fails the attempt, an inconclusive one holds it at blocked for
	// reconciliation rather than declaring it dead.
	for _, tc := range []struct {
		attemptID string
		want      domain.TaskAttemptState
	}{
		{"attempt-failed", domain.TaskAttemptStateFailed},
		{"attempt-inconclusive", domain.TaskAttemptStateBlocked},
	} {
		attempt, ok, err := s.GetTaskAttempt(ctx, tc.attemptID)
		if err != nil || !ok {
			t.Fatalf("get %s: ok=%v err=%v", tc.attemptID, ok, err)
		}
		if attempt.State != tc.want {
			t.Fatalf("%s settled to %s, want %s", tc.attemptID, attempt.State, tc.want)
		}
	}
}

// A blocked attempt is held, not dead: reconciliation must be able to move it
// forward again.
func TestTaskGraphStore_BlockedAttemptCanBeReconciled(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "proj-1")
	base := time.Now().UTC().Truncate(time.Second)
	seedTaskGraph(t, s, "proj-1", "plan-1", base)

	if _, err := s.CreateTaskAttempt(ctx, domain.TaskAttempt{
		ID: "attempt-1", PlanID: "plan-1", TaskID: "t1",
	}, base); err != nil {
		t.Fatalf("create attempt: %v", err)
	}

	if _, applied, err := s.TransitionTaskAttempt(
		ctx, "attempt-1", domain.TaskAttemptStateClaimed, domain.TaskAttemptStateRunning, nil, base.Add(time.Second)); err != nil || !applied {
		t.Fatalf("claim -> running: applied=%v err=%v", applied, err)
	}
	if _, applied, err := s.TransitionTaskAttempt(
		ctx, "attempt-1", domain.TaskAttemptStateRunning, domain.TaskAttemptStateBlocked, nil, base.Add(2*time.Second)); err != nil || !applied {
		t.Fatalf("running -> blocked: applied=%v err=%v", applied, err)
	}
	if _, applied, err := s.TransitionTaskAttempt(
		ctx, "attempt-1", domain.TaskAttemptStateBlocked, domain.TaskAttemptStateRunning, nil, base.Add(3*time.Second)); err != nil || !applied {
		t.Fatalf("blocked -> running (reconciliation): applied=%v err=%v", applied, err)
	}

	attempt, _, err := s.GetTaskAttempt(ctx, "attempt-1")
	if err != nil {
		t.Fatalf("get attempt: %v", err)
	}
	if attempt.State != domain.TaskAttemptStateRunning {
		t.Fatalf("reconciled attempt state = %s", attempt.State)
	}
	// Recovery must not erase the fact that the attempt was held.
	if attempt.FinishedAt != nil {
		t.Fatal("a held-then-reconciled attempt must not be marked finished")
	}
}

// seedWorkerSession creates a real worker session. task_attempt.session_id is a
// foreign key to sessions, so an association cannot be fabricated in a test any
// more than it can be back-mapped from a task in production.
func seedWorkerSession(t *testing.T, s *sqlite.Store, projectID string) domain.SessionID {
	t.Helper()
	rec, err := s.CreateSession(context.Background(), sampleRecord(projectID))
	if err != nil {
		t.Fatalf("seed worker session: %v", err)
	}
	return rec.ID
}

func seedTaskGraph(t *testing.T, s *sqlite.Store, projectID, planID string, now time.Time) {
	t.Helper()
	if _, err := s.CreateTaskPlan(context.Background(), domain.TaskPlan{
		ID:        planID,
		ProjectID: projectID,
		Title:     "Plan",
		Tasks:     []domain.PlannedTask{{ID: "t1", Title: "One", Prompt: "do it"}},
	}, now); err != nil {
		t.Fatalf("seed task graph: %v", err)
	}
}
