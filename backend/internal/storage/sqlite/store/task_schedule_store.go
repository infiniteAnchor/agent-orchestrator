package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// ListSchedulePlans returns every stored plan in creation order. The scheduler
// uses it to recover and to drain the ready queue; it is not a paged API.
func (s *Store) ListSchedulePlans(ctx context.Context) ([]domain.TaskPlanSummary, error) {
	rows, err := s.qr.ListAllTaskPlans(ctx)
	if err != nil {
		return nil, fmt.Errorf("list task plans: %w", err)
	}
	out := make([]domain.TaskPlanSummary, 0, len(rows))
	for _, row := range rows {
		out = append(out, domain.TaskPlanSummary{
			ID: row.ID, ProjectID: string(row.ProjectID), Title: row.Title,
			CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		})
	}
	return out, nil
}

// ListOpenTaskAttempts returns attempts that still occupy a task: claimed,
// running, collecting, or held.
func (s *Store) ListOpenTaskAttempts(ctx context.Context) ([]domain.TaskAttempt, error) {
	rows, err := s.qr.ListOpenTaskAttempts(ctx)
	if err != nil {
		return nil, fmt.Errorf("list open task attempts: %w", err)
	}
	out := make([]domain.TaskAttempt, 0, len(rows))
	for _, row := range rows {
		out = append(out, taskAttemptFromOpen(row))
	}
	return out, nil
}

// LoadTaskSchedule reads one plan's graph, attempts, verified results, and the
// project's in-flight tasks. ok is false when the plan is missing or its
// project is archived.
func (s *Store) LoadTaskSchedule(ctx context.Context, projectID domain.ProjectID, planID string) (domain.TaskScheduleView, bool, error) {
	owner, err := s.qr.ActiveProjectForPlan(ctx, planID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.TaskScheduleView{}, false, nil
	}
	if err != nil {
		return domain.TaskScheduleView{}, false, fmt.Errorf("load task schedule %s: %w", planID, err)
	}
	if owner != projectID {
		return domain.TaskScheduleView{}, false, nil
	}
	plan, err := s.assembleTaskPlan(ctx, s.qr, domain.TaskPlan{ID: planID, ProjectID: string(projectID)})
	if err != nil {
		return domain.TaskScheduleView{}, false, err
	}
	header, err := s.qr.GetTaskPlan(ctx, gen.GetTaskPlanParams{ProjectID: projectID, ID: planID})
	if err != nil {
		return domain.TaskScheduleView{}, false, fmt.Errorf("load task schedule %s: %w", planID, err)
	}
	plan.Title = header.Title
	view, err := s.scheduleView(ctx, s.qr, plan)
	if err != nil {
		return domain.TaskScheduleView{}, false, err
	}
	return view, true, nil
}

func (s *Store) scheduleView(ctx context.Context, q *gen.Queries, plan domain.TaskPlan) (domain.TaskScheduleView, error) {
	projectRow, err := q.GetProject(ctx, domain.ProjectID(plan.ProjectID))
	if err != nil {
		return domain.TaskScheduleView{}, fmt.Errorf("load project task defaults: %w", err)
	}
	defaultHarness := string(projectRowFromGen(projectRow).Config.Worker.Harness)
	taskRows, err := q.ListTasks(ctx, plan.ID)
	if err != nil {
		return domain.TaskScheduleView{}, fmt.Errorf("list tasks for plan %s: %w", plan.ID, err)
	}
	dependencyRows, err := q.ListTaskDependencies(ctx, plan.ID)
	if err != nil {
		return domain.TaskScheduleView{}, fmt.Errorf("list dependencies for plan %s: %w", plan.ID, err)
	}
	deps := make(map[string][]string, len(taskRows))
	for _, row := range dependencyRows {
		deps[row.TaskID] = append(deps[row.TaskID], row.DependsOnTaskID)
	}
	nodes := make([]domain.ScheduleNode, 0, len(taskRows))
	for _, row := range taskRows {
		if row.Harness == "" {
			row.Harness = defaultHarness
		}
		nodes = append(nodes, domain.ScheduleNode{
			PlanID: plan.ID, ID: row.ID, State: row.State,
			DependsOn:    append([]string(nil), deps[row.ID]...),
			WorkspaceKey: row.WorkspaceKey, Harness: row.Harness, Position: int(row.Position),
		})
	}
	attempts := make([]domain.TaskAttempt, 0)
	for _, node := range nodes {
		rows, err := q.ListTaskAttempts(ctx, gen.ListTaskAttemptsParams{PlanID: plan.ID, TaskID: node.ID})
		if err != nil {
			return domain.TaskScheduleView{}, fmt.Errorf("list attempts for task %s: %w", node.ID, err)
		}
		for _, row := range rows {
			attempts = append(attempts, taskAttemptFromList(row))
		}
	}
	verifiedRows, err := q.ListVerifiedTaskResults(ctx, plan.ID)
	if err != nil {
		return domain.TaskScheduleView{}, fmt.Errorf("list verified results for plan %s: %w", plan.ID, err)
	}
	verified := make(map[string]struct{}, len(verifiedRows))
	for _, row := range verifiedRows {
		verified[row.TaskID] = struct{}{}
	}
	activeRows, err := q.ListProjectActiveTasks(ctx, domain.ProjectID(plan.ProjectID))
	if err != nil {
		return domain.TaskScheduleView{}, fmt.Errorf("list active tasks for project %s: %w", plan.ProjectID, err)
	}
	active := make([]domain.ScheduleNode, 0, len(activeRows))
	for _, row := range activeRows {
		if row.Harness == "" {
			row.Harness = defaultHarness
		}
		active = append(active, domain.ScheduleNode{
			PlanID: row.PlanID, ID: row.ID, State: row.State,
			WorkspaceKey: row.WorkspaceKey, Harness: row.Harness,
		})
	}
	return domain.TaskScheduleView{
		ProjectID: plan.ProjectID, Plan: plan, Nodes: nodes,
		Attempts: attempts, Verified: verified, Active: active,
	}, nil
}

// ClaimReadyTask inserts one attempt and moves the task to claimed, or reports
// why the durable facts refuse the claim. The readiness and occupancy checks
// run inside the write transaction that performs the compare-and-swap.
func (s *Store) ClaimReadyTask(
	ctx context.Context,
	projectID domain.ProjectID,
	planID, taskID, attemptID string,
	limits domain.ScheduleLimits,
	now time.Time,
) (domain.TaskClaimResult, error) {
	if now.IsZero() || attemptID == "" {
		return domain.TaskClaimResult{}, fmt.Errorf("claim task %s: attempt id and time are required", taskID)
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return domain.TaskClaimResult{}, err
	}
	defer s.writeMu.Unlock()

	var result domain.TaskClaimResult
	err := s.inTx(ctx, "claim ready task", func(q *gen.Queries) error {
		owner, err := q.ActiveProjectForPlan(ctx, planID)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != projectID) {
			result.Reason = domain.ClaimInactivePlan
			return nil
		}
		if err != nil {
			return err
		}
		viewPlan := domain.TaskPlan{ID: planID, ProjectID: string(projectID)}
		view, err := s.scheduleView(ctx, q, viewPlan)
		if err != nil {
			return err
		}
		var node domain.ScheduleNode
		found := false
		for _, candidate := range view.Nodes {
			if candidate.ID == taskID {
				node = candidate
				found = true
				break
			}
		}
		if !found {
			result.Reason = domain.ClaimMissing
			return nil
		}
		if reason := domain.ClaimRefusal(node, view.Verified, view.Active, limits); reason != "" {
			result.Reason = reason
			return nil
		}
		if _, err := q.PendingTaskHumanGate(ctx, gen.PendingTaskHumanGateParams{PlanID: planID, TaskID: taskID}); err == nil {
			result.Reason = domain.ClaimHumanGate
			return nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		rows, err := q.TransitionTaskState(ctx, gen.TransitionTaskStateParams{
			State: domain.TaskStateClaimed, UpdatedAt: now, PlanID: planID, ID: taskID, State_2: domain.TaskStateQueued,
		})
		if err != nil {
			return err
		}
		if rows == 0 {
			result.Reason = domain.ClaimState
			return nil
		}
		number, err := q.NextTaskAttemptNumber(ctx, gen.NextTaskAttemptNumberParams{PlanID: planID, TaskID: taskID})
		if err != nil {
			return err
		}
		attempt := domain.TaskAttempt{
			ID: attemptID, PlanID: planID, TaskID: taskID, AttemptNumber: int(number),
			State: domain.TaskAttemptStateClaimed, Harness: domain.AgentHarness(node.Harness),
			ClaimedAt: now, CreatedAt: now, UpdatedAt: now,
		}
		if err := attempt.Validate(); err != nil {
			return err
		}
		if err := q.InsertTaskAttempt(ctx, gen.InsertTaskAttemptParams{
			ID: attempt.ID, PlanID: attempt.PlanID, TaskID: attempt.TaskID,
			AttemptNumber: int64(attempt.AttemptNumber), State: attempt.State,
			SessionID: nil, Harness: nullableHarness(attempt.Harness), RuntimeRef: "",
			ClaimedAt: now, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return err
		}
		result.Attempt = attempt
		return nil
	})
	if err != nil {
		return domain.TaskClaimResult{}, err
	}
	return result, nil
}

// LeaseAttemptDispatch marks a claimed attempt so a second dispatcher cannot
// launch it. The lease is not a runtime identity.
func (s *Store) LeaseAttemptDispatch(ctx context.Context, attemptID string, at time.Time) (bool, error) {
	if at.IsZero() {
		return false, fmt.Errorf("lease attempt %s: time is required", attemptID)
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return false, err
	}
	defer s.writeMu.Unlock()
	rows, err := s.qw.LeaseTaskAttemptDispatch(ctx, gen.LeaseTaskAttemptDispatchParams{UpdatedAt: at, ID: attemptID})
	if err != nil {
		return false, fmt.Errorf("lease attempt %s: %w", attemptID, err)
	}
	return rows > 0, nil
}

// ReleaseAttemptDispatch clears a lease after the launcher confirms it did not
// start a worker, so the same attempt can be dispatched again.
func (s *Store) ReleaseAttemptDispatch(ctx context.Context, attemptID string, at time.Time) (bool, error) {
	if at.IsZero() {
		return false, fmt.Errorf("release attempt %s: time is required", attemptID)
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return false, err
	}
	defer s.writeMu.Unlock()
	rows, err := s.qw.ReleaseTaskAttemptDispatch(ctx, gen.ReleaseTaskAttemptDispatchParams{UpdatedAt: at, ID: attemptID})
	if err != nil {
		return false, fmt.Errorf("release attempt %s: %w", attemptID, err)
	}
	return rows > 0, nil
}

// FinishAttemptLaunch records the launcher identity and moves the claimed
// attempt and its task to running. A foreign-key failure rolls the lease back
// with the transaction; the caller must hold the attempt rather than retry
// with a new id.
func (s *Store) FinishAttemptLaunch(
	ctx context.Context,
	attemptID, runtimeRef, sessionID string,
	harness domain.AgentHarness,
	at time.Time,
) (domain.TaskAttempt, bool, error) {
	if at.IsZero() {
		return domain.TaskAttempt{}, false, fmt.Errorf("finish attempt %s: time is required", attemptID)
	}
	if err := domain.ValidateRuntimeRef(runtimeRef); err != nil || runtimeRef == "" || runtimeRef == domain.TaskAttemptDispatchLease {
		return domain.TaskAttempt{}, false, fmt.Errorf("finish attempt %s: runtime ref is not recordable", attemptID)
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return domain.TaskAttempt{}, false, err
	}
	defer s.writeMu.Unlock()

	var updated domain.TaskAttempt
	applied := false
	err := s.inTx(ctx, "finish attempt launch", func(q *gen.Queries) error {
		current, err := q.GetTaskAttempt(ctx, attemptID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		attempt := taskAttemptFromGet(current)
		if attempt.State == domain.TaskAttemptStateRunning && attempt.RuntimeRef == runtimeRef {
			updated = attempt
			applied = true
			return nil
		}
		if attempt.State != domain.TaskAttemptStateClaimed {
			return nil
		}
		session := nullableSessionID(attempt.SessionID)
		if sessionID != "" {
			session = nullableSessionID(sessionID)
		}
		boundHarness := nullableHarness(attempt.Harness)
		if harness != "" {
			boundHarness = nullableHarness(harness)
		}
		rows, err := q.BindTaskAttemptRuntime(ctx, gen.BindTaskAttemptRuntimeParams{
			RuntimeRef: runtimeRef, SessionID: session, Harness: boundHarness, UpdatedAt: at, ID: attemptID,
		})
		if err != nil {
			return err
		}
		if rows == 0 {
			return nil
		}
		settled, ok, err := settleTaskAttempt(ctx, q, attemptID, domain.TaskAttemptStateClaimed, domain.TaskAttemptStateRunning, session, at)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("finish attempt %s: claim changed before launch was recorded", attemptID)
		}
		task, err := q.GetTask(ctx, gen.GetTaskParams{PlanID: settled.PlanID, ID: settled.TaskID})
		if err != nil {
			return err
		}
		if err := applyTaskSteps(ctx, q, settled.PlanID, settled.TaskID, task.State, domain.TaskStateRunning, at); err != nil {
			return err
		}
		updated = settled
		updated.RuntimeRef = runtimeRef
		if harness != "" {
			updated.Harness = harness
		}
		if sessionID != "" {
			updated.SessionID = sessionID
		}
		applied = true
		return nil
	})
	if err != nil {
		return domain.TaskAttempt{}, false, err
	}
	return updated, applied, nil
}

// HoldAttempt keeps an ambiguous dispatch from being replaced. The attempt and
// its task move to blocked when that walk is legal.
func (s *Store) HoldAttempt(ctx context.Context, attemptID string, at time.Time) (bool, error) {
	return s.settleAttemptAndTask(ctx, attemptID, domain.TaskAttemptStateBlocked, domain.TaskStateBlocked, at)
}

// FailAttemptLaunch records a launcher failure that is known not to have
// started a worker. It does not create a replacement attempt.
func (s *Store) FailAttemptLaunch(ctx context.Context, attemptID string, at time.Time) (bool, error) {
	return s.settleAttemptAndTask(ctx, attemptID, domain.TaskAttemptStateFailed, domain.TaskStateFailed, at)
}

func (s *Store) settleAttemptAndTask(ctx context.Context, attemptID string, attemptTo domain.TaskAttemptState, taskTo domain.TaskState, at time.Time) (bool, error) {
	if at.IsZero() {
		return false, fmt.Errorf("settle attempt %s: time is required", attemptID)
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return false, err
	}
	defer s.writeMu.Unlock()
	applied := false
	err := s.inTx(ctx, "settle attempt", func(q *gen.Queries) error {
		current, err := q.GetTaskAttempt(ctx, attemptID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		attempt := taskAttemptFromGet(current)
		if attempt.State == attemptTo {
			applied = true
		} else if _, ok := domain.CheckAdvanceAttempt(attempt.State, attemptTo); !ok {
			return nil
		} else if _, err := applyAttemptSteps(ctx, q, attempt, attemptTo, at); err != nil {
			return err
		} else {
			applied = true
		}
		task, err := q.GetTask(ctx, gen.GetTaskParams{PlanID: attempt.PlanID, ID: attempt.TaskID})
		if err != nil {
			return err
		}
		return applyTaskSteps(ctx, q, attempt.PlanID, attempt.TaskID, task.State, taskTo, at)
	})
	if err != nil {
		return false, err
	}
	return applied, nil
}

// BeginCollection moves a running attempt and task into collecting. An attempt
// that is already collecting is left in place so verification can resume.
func (s *Store) BeginCollection(ctx context.Context, planID, taskID, attemptID string, at time.Time) (bool, error) {
	if at.IsZero() {
		return false, fmt.Errorf("begin collection %s: time is required", attemptID)
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return false, err
	}
	defer s.writeMu.Unlock()
	applied := false
	err := s.inTx(ctx, "begin collection", func(q *gen.Queries) error {
		current, err := q.GetTaskAttempt(ctx, attemptID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		attempt := taskAttemptFromGet(current)
		if attempt.PlanID != planID || attempt.TaskID != taskID {
			return nil
		}
		if attempt.State != domain.TaskAttemptStateCollecting {
			if _, err := applyAttemptSteps(ctx, q, attempt, domain.TaskAttemptStateCollecting, at); err != nil {
				return err
			}
		}
		task, err := q.GetTask(ctx, gen.GetTaskParams{PlanID: planID, ID: taskID})
		if err != nil {
			return err
		}
		if err := applyTaskSteps(ctx, q, planID, taskID, task.State, domain.TaskStateCollecting, at); err != nil {
			return err
		}
		applied = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return applied, nil
}

// CommitTaskResult appends verification evidence and moves the attempt and task
// to the outcome's states in one transaction.
func (s *Store) CommitTaskResult(ctx context.Context, result domain.TaskResult) (bool, error) {
	if err := result.Validate(); err != nil {
		return false, err
	}
	settled := domain.AttemptStateForOutcome(result.Outcome)
	taskTo := taskStateForOutcome(result.Outcome)
	if settled == "" || taskTo == "" {
		return false, fmt.Errorf("commit task result: outcome %q has no state", result.Outcome)
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return false, err
	}
	defer s.writeMu.Unlock()
	applied := false
	err := s.inTx(ctx, "commit task result", func(q *gen.Queries) error {
		existing, err := q.GetTaskResultByAttempt(ctx, result.AttemptID)
		if err == nil {
			if existing.Outcome != result.Outcome || existing.PlanID != result.PlanID || existing.TaskID != result.TaskID {
				return fmt.Errorf("commit task result: attempt %s already has outcome %s", result.AttemptID, existing.Outcome)
			}
			task, err := q.GetTask(ctx, gen.GetTaskParams{PlanID: result.PlanID, ID: result.TaskID})
			if err != nil {
				return err
			}
			applied = true
			return applyTaskSteps(ctx, q, result.PlanID, result.TaskID, task.State, taskTo, result.RecordedAt)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		current, err := q.GetTaskAttempt(ctx, result.AttemptID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		attempt := taskAttemptFromGet(current)
		if attempt.PlanID != result.PlanID || attempt.TaskID != result.TaskID {
			return fmt.Errorf("commit task result: attempt %s is not task %s", result.AttemptID, result.TaskID)
		}
		if attempt.State != domain.TaskAttemptStateCollecting {
			if _, err := applyAttemptSteps(ctx, q, attempt, domain.TaskAttemptStateCollecting, result.RecordedAt); err != nil {
				return err
			}
		}
		if _, ok, err := settleTaskAttempt(ctx, q, result.AttemptID, domain.TaskAttemptStateCollecting, settled, nil, result.RecordedAt); err != nil {
			return err
		} else if !ok {
			return fmt.Errorf("commit task result: attempt %s changed before the result was recorded", result.AttemptID)
		}
		if err := q.InsertTaskResult(ctx, gen.InsertTaskResultParams{
			ID: result.ID, PlanID: result.PlanID, TaskID: result.TaskID, AttemptID: result.AttemptID,
			Outcome: result.Outcome, Summary: nullableString(result.Summary), Evidence: string(result.Evidence),
			RecordedAt: result.RecordedAt,
		}); err != nil {
			return err
		}
		task, err := q.GetTask(ctx, gen.GetTaskParams{PlanID: result.PlanID, ID: result.TaskID})
		if err != nil {
			return err
		}
		if err := applyTaskSteps(ctx, q, result.PlanID, result.TaskID, task.State, taskTo, result.RecordedAt); err != nil {
			return err
		}
		applied = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return applied, nil
}

// AlignTask moves a task onto the state implied by its latest attempt and any
// recorded result. It is how recovery finishes a completion that was durable
// for the attempt before the task row caught up.
func (s *Store) AlignTask(ctx context.Context, planID, taskID string, at time.Time) error {
	if at.IsZero() {
		return fmt.Errorf("align task %s: time is required", taskID)
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "align task", func(q *gen.Queries) error {
		task, err := q.GetTask(ctx, gen.GetTaskParams{PlanID: planID, ID: taskID})
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		attempts, err := q.ListTaskAttempts(ctx, gen.ListTaskAttemptsParams{PlanID: planID, TaskID: taskID})
		if err != nil {
			return err
		}
		if len(attempts) == 0 {
			return nil
		}
		latest := taskAttemptFromList(attempts[len(attempts)-1])
		target := task.State
		if result, err := q.GetTaskResultByAttempt(ctx, latest.ID); err == nil {
			if outcomeState := taskStateForOutcome(result.Outcome); outcomeState != "" {
				target = outcomeState
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		} else {
			switch latest.State {
			case domain.TaskAttemptStateRunning:
				target = domain.TaskStateRunning
			case domain.TaskAttemptStateCollecting:
				target = domain.TaskStateCollecting
			case domain.TaskAttemptStateBlocked:
				target = domain.TaskStateBlocked
			case domain.TaskAttemptStateFailed:
				target = domain.TaskStateFailed
			case domain.TaskAttemptStateClaimed:
				target = domain.TaskStateClaimed
			}
		}
		return applyTaskSteps(ctx, q, planID, taskID, task.State, target, at)
	})
}

func taskStateForOutcome(outcome domain.TaskResultOutcome) domain.TaskState {
	switch outcome {
	case domain.TaskResultVerified:
		return domain.TaskStateCompleted
	case domain.TaskResultFailed:
		return domain.TaskStateFailed
	case domain.TaskResultInconclusive:
		return domain.TaskStateBlocked
	default:
		return ""
	}
}

func applyTaskSteps(ctx context.Context, q *gen.Queries, planID, taskID string, from, to domain.TaskState, at time.Time) error {
	steps, ok := domain.CheckAdvanceTask(from, to)
	if !ok {
		return fmt.Errorf("task %s cannot advance %s -> %s", taskID, from, to)
	}
	cur := from
	for _, step := range steps {
		rows, err := q.TransitionTaskState(ctx, gen.TransitionTaskStateParams{
			State: step, UpdatedAt: at, PlanID: planID, ID: taskID, State_2: cur,
		})
		if err != nil {
			return err
		}
		if rows == 0 {
			return fmt.Errorf("task %s changed before %s -> %s", taskID, cur, step)
		}
		cur = step
	}
	return nil
}

func applyAttemptSteps(ctx context.Context, q *gen.Queries, attempt domain.TaskAttempt, to domain.TaskAttemptState, at time.Time) (domain.TaskAttempt, error) {
	steps, ok := domain.CheckAdvanceAttempt(attempt.State, to)
	if !ok {
		return domain.TaskAttempt{}, fmt.Errorf("attempt %s cannot advance %s -> %s", attempt.ID, attempt.State, to)
	}
	cur := attempt
	for _, step := range steps {
		next, applied, err := settleTaskAttempt(ctx, q, cur.ID, cur.State, step, nil, at)
		if err != nil {
			return domain.TaskAttempt{}, err
		}
		if !applied {
			return domain.TaskAttempt{}, fmt.Errorf("attempt %s changed before %s -> %s", cur.ID, cur.State, step)
		}
		cur = next
	}
	return cur, nil
}
