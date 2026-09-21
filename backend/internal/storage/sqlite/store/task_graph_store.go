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

// This file is the durable task graph (slice 2 of
// docs/plans/phase-3-durable-task-graph.md). It persists graphs, attempts, and
// results; it does NOT schedule, dispatch, or decide readiness. Readiness is
// derived from ListVerifiedTaskResults plus the dependency edges by whichever
// slice owns the ready queue.
//
// Two invariants hold here, both enforced by the schema as well as by this file:
//
//   - Task results are append-only. There is no update or delete method, and
//     migration 0129's triggers abort both at the SQLite level.
//   - Attempt transitions are compare-and-swap on the current state. Every
//     transition names the state it expects to move FROM, so a losing concurrent
//     claim updates zero rows instead of double-dispatching a task.

// CreateTaskPlan validates a graph and persists it atomically: either every
// phase, task, dependency, and verification command is written, or none is.
// Validation runs before any row is written, so an invalid graph cannot leave a
// partial plan behind.
//
// The insert selects an active project while holding the store write lock, so a
// concurrent archive cannot slip between the service's ownership check and the
// durable write. A missing or archived project returns
// domain.ErrTaskPlanProjectNotFound.
func (s *Store) CreateTaskPlan(ctx context.Context, plan domain.TaskPlan, now time.Time) (domain.TaskPlanSummary, error) {
	if err := plan.Validate(); err != nil {
		return domain.TaskPlanSummary{}, err
	}
	if now.IsZero() {
		return domain.TaskPlanSummary{}, fmt.Errorf("create task plan: now is required")
	}

	if err := s.writeMu.LockContext(ctx); err != nil {
		return domain.TaskPlanSummary{}, err
	}
	defer s.writeMu.Unlock()

	err := s.inTx(ctx, "create task plan", func(q *gen.Queries) error {
		inserted, err := q.InsertTaskPlan(ctx, gen.InsertTaskPlanParams{
			ID:        plan.ID,
			ProjectID: domain.ProjectID(plan.ProjectID),
			Title:     plan.Title,
			CreatedAt: now,
			UpdatedAt: now,
		})
		if err != nil {
			return fmt.Errorf("insert task plan %s: %w", plan.ID, err)
		}
		if inserted == 0 {
			return fmt.Errorf("insert task plan %s: %w", plan.ID, domain.ErrTaskPlanProjectNotFound)
		}

		for i, phase := range plan.Phases {
			if err := q.InsertTaskPhase(ctx, gen.InsertTaskPhaseParams{
				PlanID:   plan.ID,
				ID:       phase.ID,
				Title:    phase.Title,
				Position: int64(i),
			}); err != nil {
				return fmt.Errorf("insert task phase %s: %w", phase.ID, err)
			}
		}

		// Tasks are inserted in a first pass, and edges and commands in a second.
		// A plan is explicitly allowed to declare a task before the tasks it
		// depends on ("out-of-order DAGs" in the Phase 3 plan), and the composite
		// FK from task_dependency to task would reject the edge if the target row
		// did not exist yet.
		for i, task := range plan.Tasks {
			var phaseID sql.NullString
			if task.PhaseID != "" {
				phaseID = sql.NullString{String: task.PhaseID, Valid: true}
			}
			if err := q.InsertTask(ctx, gen.InsertTaskParams{
				PlanID:       plan.ID,
				ID:           task.ID,
				PhaseID:      phaseID,
				Title:        task.Title,
				Prompt:       task.Prompt,
				WorkspaceKey: task.WorkspaceKey,
				Harness:      task.Harness,
				Position:     int64(i),
				State:        domain.TaskStateQueued,
				CreatedAt:    now,
				UpdatedAt:    now,
			}); err != nil {
				return fmt.Errorf("insert task %s: %w", task.ID, err)
			}
		}

		for _, task := range plan.Tasks {
			for j, dependency := range task.DependsOn {
				if err := q.InsertTaskDependency(ctx, gen.InsertTaskDependencyParams{
					PlanID:          plan.ID,
					TaskID:          task.ID,
					DependsOnTaskID: dependency,
					Position:        int64(j),
				}); err != nil {
					return fmt.Errorf("insert task %s dependency %s: %w", task.ID, dependency, err)
				}
			}
			for j, command := range task.VerificationCommands {
				if err := q.InsertTaskVerificationCommand(ctx, gen.InsertTaskVerificationCommandParams{
					PlanID:   plan.ID,
					TaskID:   task.ID,
					Position: int64(j),
					Command:  command,
				}); err != nil {
					return fmt.Errorf("insert task %s verification command %d: %w", task.ID, j, err)
				}
			}
		}
		return nil
	})
	if err != nil {
		if isSQLiteUnique(err) || isSQLitePrimaryKey(err) {
			return domain.TaskPlanSummary{}, fmt.Errorf("create task plan %s: %w", plan.ID, domain.ErrDuplicateTaskPlan)
		}
		return domain.TaskPlanSummary{}, err
	}
	return domain.TaskPlanSummary{
		ID:        plan.ID,
		ProjectID: plan.ProjectID,
		Title:     plan.Title,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// GetTaskPlan returns one stored plan's full graph, ok=false if the plan does
// not exist for that project. Scoping by project means a caller cannot read
// another project's graph by guessing a plan id.
func (s *Store) GetTaskPlan(ctx context.Context, projectID domain.ProjectID, planID string) (domain.TaskPlan, bool, error) {
	row, err := s.qr.GetTaskPlan(ctx, gen.GetTaskPlanParams{ProjectID: projectID, ID: planID})
	if errors.Is(err, sql.ErrNoRows) {
		return domain.TaskPlan{}, false, nil
	}
	if err != nil {
		return domain.TaskPlan{}, false, fmt.Errorf("get task plan %s: %w", planID, err)
	}
	plan, err := s.assembleTaskPlan(ctx, s.qr, domain.TaskPlan{
		ID:        row.ID,
		ProjectID: string(row.ProjectID),
		Title:     row.Title,
	})
	if err != nil {
		return domain.TaskPlan{}, false, err
	}
	return plan, true, nil
}

// ListTaskPlans returns a project's plan metadata, newest first. It deliberately
// returns summaries rather than graphs: materialising every task, edge, and
// command for a whole project is unbounded work, and the list surface does not
// need it.
func (s *Store) ListTaskPlans(
	ctx context.Context,
	projectID domain.ProjectID,
	beforeCreatedAt time.Time,
	beforeID string,
	limit int,
) ([]domain.TaskPlanSummary, error) {
	rows, err := s.qr.ListTaskPlansPage(ctx, gen.ListTaskPlansPageParams{
		ProjectID:       projectID,
		BeforeCreatedAt: beforeCreatedAt,
		BeforeID:        beforeID,
		PageLimit:       int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("list task plans for project %s: %w", projectID, err)
	}
	out := make([]domain.TaskPlanSummary, 0, len(rows))
	for _, row := range rows {
		out = append(out, domain.TaskPlanSummary{
			ID:        row.ID,
			ProjectID: string(row.ProjectID),
			Title:     row.Title,
			CreatedAt: row.CreatedAt,
			UpdatedAt: row.UpdatedAt,
		})
	}
	return out, nil
}

// assembleTaskPlan fills in a plan's phases, tasks, dependencies, and
// verification commands. Every child query orders by the stored `position`, so
// the round-tripped graph preserves the order the caller declared rather than
// whatever order SQLite happens to return.
func (s *Store) assembleTaskPlan(ctx context.Context, q *gen.Queries, plan domain.TaskPlan) (domain.TaskPlan, error) {
	phaseRows, err := q.ListTaskPhases(ctx, plan.ID)
	if err != nil {
		return domain.TaskPlan{}, fmt.Errorf("list task phases for plan %s: %w", plan.ID, err)
	}
	plan.Phases = make([]domain.TaskPhase, 0, len(phaseRows))
	for _, row := range phaseRows {
		plan.Phases = append(plan.Phases, domain.TaskPhase{ID: row.ID, Title: row.Title})
	}

	taskRows, err := q.ListTasks(ctx, plan.ID)
	if err != nil {
		return domain.TaskPlan{}, fmt.Errorf("list tasks for plan %s: %w", plan.ID, err)
	}
	dependencyRows, err := q.ListTaskDependencies(ctx, plan.ID)
	if err != nil {
		return domain.TaskPlan{}, fmt.Errorf("list task dependencies for plan %s: %w", plan.ID, err)
	}
	commandRows, err := q.ListTaskVerificationCommands(ctx, plan.ID)
	if err != nil {
		return domain.TaskPlan{}, fmt.Errorf("list task verification commands for plan %s: %w", plan.ID, err)
	}

	dependenciesByTask := make(map[string][]string, len(taskRows))
	for _, row := range dependencyRows {
		dependenciesByTask[row.TaskID] = append(dependenciesByTask[row.TaskID], row.DependsOnTaskID)
	}
	commandsByTask := make(map[string][]string, len(taskRows))
	for _, row := range commandRows {
		commandsByTask[row.TaskID] = append(commandsByTask[row.TaskID], row.Command)
	}

	plan.Tasks = make([]domain.PlannedTask, 0, len(taskRows))
	for _, row := range taskRows {
		plan.Tasks = append(plan.Tasks, domain.PlannedTask{
			ID:                   row.ID,
			PhaseID:              row.PhaseID.String,
			Title:                row.Title,
			Prompt:               row.Prompt,
			DependsOn:            dependenciesByTask[row.ID],
			VerificationCommands: commandsByTask[row.ID],
			WorkspaceKey:         row.WorkspaceKey,
			Harness:              row.Harness,
		})
	}
	return plan, nil
}

// TransitionTask moves a task between lifecycle states, compare-and-swap on
// from. It returns applied=false when the task is not currently in `from` (a
// concurrent transition won, or the caller's view was stale) — that is not an
// error, and it is how the ready-queue prevents two dispatchers claiming one
// task.
func (s *Store) TransitionTask(ctx context.Context, planID, taskID string, from, to domain.TaskState, at time.Time) (bool, error) {
	if !domain.CanTransitionTask(from, to) {
		return false, fmt.Errorf("transition task %s: %s -> %s is not a legal transition", taskID, from, to)
	}
	if at.IsZero() {
		return false, fmt.Errorf("transition task %s: at is required", taskID)
	}

	if err := s.writeMu.LockContext(ctx); err != nil {
		return false, err
	}
	defer s.writeMu.Unlock()

	rows, err := s.qw.TransitionTaskState(ctx, gen.TransitionTaskStateParams{
		State:     to,
		UpdatedAt: at,
		PlanID:    planID,
		ID:        taskID,
		State_2:   from,
	})
	if err != nil {
		return false, fmt.Errorf("transition task %s: %w", taskID, err)
	}
	return rows > 0, nil
}

// CreateTaskAttempt persists a dispatch identity. This is the row that makes a
// crash between claim and launch reconcilable: it exists BEFORE the runtime is
// started, so recovery can adopt the attempt by identity instead of launching a
// duplicate worker.
//
// An attempt is normally claimed with no session and no harness yet; both are
// resolved later by TransitionTaskAttempt. AttemptNumber may be left zero to
// allocate the next number for the task inside this transaction.
func (s *Store) CreateTaskAttempt(ctx context.Context, attempt domain.TaskAttempt, now time.Time) (domain.TaskAttempt, error) {
	if now.IsZero() {
		return domain.TaskAttempt{}, fmt.Errorf("create task attempt: now is required")
	}
	if attempt.CreatedAt.IsZero() {
		attempt.CreatedAt = now
	}
	if attempt.UpdatedAt.IsZero() {
		attempt.UpdatedAt = now
	}
	if attempt.ClaimedAt.IsZero() {
		attempt.ClaimedAt = now
	}
	if attempt.State == "" {
		attempt.State = domain.TaskAttemptStateClaimed
	}

	if err := s.writeMu.LockContext(ctx); err != nil {
		return domain.TaskAttempt{}, err
	}
	defer s.writeMu.Unlock()

	// Allocation, validation, and insert share one transaction: the attempt
	// number is only valid once allocated, and validating a zero number outside
	// the transaction would reject every auto-numbered claim.
	err := s.inTx(ctx, "create task attempt", func(q *gen.Queries) error {
		if attempt.AttemptNumber == 0 {
			number, err := q.NextTaskAttemptNumber(ctx, gen.NextTaskAttemptNumberParams{
				PlanID: attempt.PlanID,
				TaskID: attempt.TaskID,
			})
			if err != nil {
				return fmt.Errorf("next attempt number for task %s: %w", attempt.TaskID, err)
			}
			attempt.AttemptNumber = int(number)
		}
		if err := attempt.Validate(); err != nil {
			return err
		}
		return q.InsertTaskAttempt(ctx, gen.InsertTaskAttemptParams{
			ID:            attempt.ID,
			PlanID:        attempt.PlanID,
			TaskID:        attempt.TaskID,
			AttemptNumber: int64(attempt.AttemptNumber),
			State:         attempt.State,
			SessionID:     nullableSessionID(attempt.SessionID),
			Harness:       nullableHarness(attempt.Harness),
			RuntimeRef:    attempt.RuntimeRef,
			ClaimedAt:     attempt.ClaimedAt,
			StartedAt:     nullableTime(attempt.StartedAt),
			FinishedAt:    nullableTime(attempt.FinishedAt),
			CreatedAt:     attempt.CreatedAt,
			UpdatedAt:     attempt.UpdatedAt,
		})
	})
	if err != nil {
		return domain.TaskAttempt{}, err
	}
	return attempt, nil
}

// GetTaskAttempt returns one attempt, ok=false if it does not exist.
func (s *Store) GetTaskAttempt(ctx context.Context, attemptID string) (domain.TaskAttempt, bool, error) {
	row, err := s.qr.GetTaskAttempt(ctx, attemptID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.TaskAttempt{}, false, nil
	}
	if err != nil {
		return domain.TaskAttempt{}, false, fmt.Errorf("get task attempt %s: %w", attemptID, err)
	}
	return taskAttemptFromGet(row), true, nil
}

// ListTaskAttempts returns a task's attempts in claim order.
func (s *Store) ListTaskAttempts(ctx context.Context, planID, taskID string) ([]domain.TaskAttempt, error) {
	rows, err := s.qr.ListTaskAttempts(ctx, gen.ListTaskAttemptsParams{PlanID: planID, TaskID: taskID})
	if err != nil {
		return nil, fmt.Errorf("list task attempts for task %s: %w", taskID, err)
	}
	out := make([]domain.TaskAttempt, 0, len(rows))
	for _, row := range rows {
		out = append(out, taskAttemptFromList(row))
	}
	return out, nil
}

// TransitionTaskAttempt moves an attempt between lifecycle states,
// compare-and-swap on from, in one transaction with a re-read of the current
// row. The re-read is what makes the CAS meaningful: the caller's from is
// checked against durable state, not against its own copy.
//
// sessionID is the durable association of a worker session with this attempt.
// Pass nil to leave the existing association untouched. It is never derived from
// the task — a task has no session.
func (s *Store) TransitionTaskAttempt(
	ctx context.Context,
	attemptID string,
	from, to domain.TaskAttemptState,
	sessionID *domain.SessionID,
	at time.Time,
) (domain.TaskAttempt, bool, error) {
	if !domain.CanTransitionTaskAttempt(from, to) {
		return domain.TaskAttempt{}, false, fmt.Errorf("transition task attempt %s: %s -> %s is not a legal transition", attemptID, from, to)
	}
	if at.IsZero() {
		return domain.TaskAttempt{}, false, fmt.Errorf("transition task attempt %s: at is required", attemptID)
	}

	if err := s.writeMu.LockContext(ctx); err != nil {
		return domain.TaskAttempt{}, false, err
	}
	defer s.writeMu.Unlock()

	var updated domain.TaskAttempt
	applied := false
	err := s.inTx(ctx, "transition task attempt", func(q *gen.Queries) error {
		var err error
		updated, applied, err = settleTaskAttempt(ctx, q, attemptID, from, to, sessionID, at)
		return err
	})
	if err != nil {
		return domain.TaskAttempt{}, false, err
	}
	return updated, applied, nil
}

// settleTaskAttempt applies one guarded transition to a durable attempt row
// inside an existing transaction.
//
// It re-reads the row and writes back the whole record, so a field the caller
// did not supply (the session association, started_at) is preserved rather than
// nulled out. That matters for results: the CDC trigger and the ready queue both
// attribute an attempt to its worker session, and a settle path that cleared
// session_id would silently drop that association at the moment the evidence
// arrives.
func settleTaskAttempt(
	ctx context.Context,
	q *gen.Queries,
	attemptID string,
	from, to domain.TaskAttemptState,
	sessionID *domain.SessionID,
	at time.Time,
) (domain.TaskAttempt, bool, error) {
	row, err := q.GetTaskAttempt(ctx, attemptID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.TaskAttempt{}, false, nil
	}
	if err != nil {
		return domain.TaskAttempt{}, false, fmt.Errorf("read task attempt %s: %w", attemptID, err)
	}
	current := taskAttemptFromGet(row)
	if current.State != from {
		return domain.TaskAttempt{}, false, nil
	}

	next := current
	next.State = to
	next.UpdatedAt = at
	if sessionID != nil {
		next.SessionID = string(*sessionID)
	}
	switch {
	case to == domain.TaskAttemptStateRunning:
		if next.StartedAt == nil {
			started := at
			next.StartedAt = &started
		}
	case to.Terminal():
		finished := at
		next.FinishedAt = &finished
	}

	rows, err := q.TransitionTaskAttempt(ctx, gen.TransitionTaskAttemptParams{
		State:      next.State,
		SessionID:  nullableSessionID(next.SessionID),
		Harness:    nullableHarness(next.Harness),
		RuntimeRef: next.RuntimeRef,
		StartedAt:  nullableTime(next.StartedAt),
		FinishedAt: nullableTime(next.FinishedAt),
		UpdatedAt:  next.UpdatedAt,
		ID:         attemptID,
		State_2:    from,
	})
	if err != nil {
		return domain.TaskAttempt{}, false, fmt.Errorf("transition task attempt %s: %w", attemptID, err)
	}
	if rows == 0 {
		return domain.TaskAttempt{}, false, nil
	}
	return next, true, nil
}

// RecordTaskResult appends an immutable verification result and settles its
// attempt in the same transaction, so evidence and the state it justifies can
// never disagree.
//
// attemptFrom is the state the attempt must currently be in; the transition to
// the outcome's state is checked with domain.CanTransitionTaskAttempt. A
// terminal attempt is rejected: an attempt settles once, which is what makes
// "verified" unambiguous when deriving readiness.
//
// The insert is append-only. There is no update or delete counterpart, and the
// schema's triggers abort both.
func (s *Store) RecordTaskResult(ctx context.Context, result domain.TaskResult, attemptFrom domain.TaskAttemptState) (domain.TaskResult, bool, error) {
	if err := result.Validate(); err != nil {
		return domain.TaskResult{}, false, err
	}
	if attemptFrom.Terminal() {
		return domain.TaskResult{}, false, fmt.Errorf("record task result: attempt state %s is already terminal", attemptFrom)
	}
	settled := domain.AttemptStateForOutcome(result.Outcome)
	if settled == "" {
		return domain.TaskResult{}, false, fmt.Errorf("record task result: outcome %q has no attempt state", result.Outcome)
	}
	if !domain.CanTransitionTaskAttempt(attemptFrom, settled) {
		return domain.TaskResult{}, false, fmt.Errorf("record task result: attempt transition %s -> %s is not legal", attemptFrom, settled)
	}

	if err := s.writeMu.LockContext(ctx); err != nil {
		return domain.TaskResult{}, false, err
	}
	defer s.writeMu.Unlock()

	applied := false
	err := s.inTx(ctx, "record task result", func(q *gen.Queries) error {
		settledAttempt, ok, err := settleTaskAttempt(ctx, q, result.AttemptID, attemptFrom, settled, nil, result.RecordedAt)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		// A result must describe the attempt it settles. Readiness is derived per
		// task, so a mis-attributed result would unlock the wrong task's
		// dependents. The schema's foreign keys cannot express this: they tie the
		// result to a task and to an attempt independently, not the two together.
		if settledAttempt.PlanID != result.PlanID || settledAttempt.TaskID != result.TaskID {
			return fmt.Errorf(
				"record task result: attempt %s belongs to task %s in plan %s, not task %s in plan %s",
				result.AttemptID, settledAttempt.TaskID, settledAttempt.PlanID, result.TaskID, result.PlanID,
			)
		}
		if err := q.InsertTaskResult(ctx, gen.InsertTaskResultParams{
			ID:         result.ID,
			PlanID:     result.PlanID,
			TaskID:     result.TaskID,
			AttemptID:  result.AttemptID,
			Outcome:    result.Outcome,
			Summary:    nullableString(result.Summary),
			Evidence:   string(result.Evidence),
			RecordedAt: result.RecordedAt,
		}); err != nil {
			return fmt.Errorf("insert task result %s: %w", result.ID, err)
		}
		applied = true
		return nil
	})
	if err != nil {
		return domain.TaskResult{}, false, err
	}
	return result, applied, nil
}

// GetTaskResultByAttempt returns the single result recorded for an attempt,
// ok=false if the attempt has not settled with evidence yet.
func (s *Store) GetTaskResultByAttempt(ctx context.Context, attemptID string) (domain.TaskResult, bool, error) {
	row, err := s.qr.GetTaskResultByAttempt(ctx, attemptID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.TaskResult{}, false, nil
	}
	if err != nil {
		return domain.TaskResult{}, false, fmt.Errorf("get task result for attempt %s: %w", attemptID, err)
	}
	return taskResultFromRow(row), true, nil
}

// ListTaskResults returns every recorded result for a plan, oldest first.
func (s *Store) ListTaskResults(ctx context.Context, planID string) ([]domain.TaskResult, error) {
	rows, err := s.qr.ListTaskResults(ctx, planID)
	if err != nil {
		return nil, fmt.Errorf("list task results for plan %s: %w", planID, err)
	}
	out := make([]domain.TaskResult, 0, len(rows))
	for _, row := range rows {
		out = append(out, taskResultFromRow(row))
	}
	return out, nil
}

// ListVerifiedTaskResults returns only the verified results for a plan. This is
// the durable fact the ready queue derives from: only outcome='verified' unlocks
// a dependent task, and an inconclusive result is held rather than read as
// either success or failure.
func (s *Store) ListVerifiedTaskResults(ctx context.Context, planID string) ([]domain.TaskResult, error) {
	rows, err := s.qr.ListVerifiedTaskResults(ctx, planID)
	if err != nil {
		return nil, fmt.Errorf("list verified task results for plan %s: %w", planID, err)
	}
	out := make([]domain.TaskResult, 0, len(rows))
	for _, row := range rows {
		out = append(out, taskResultFromRow(row))
	}
	return out, nil
}

func taskAttemptFromGet(row gen.GetTaskAttemptRow) domain.TaskAttempt {
	return taskAttemptFromFields(row.ID, row.PlanID, row.TaskID, row.AttemptNumber, row.State, row.SessionID, row.Harness, row.RuntimeRef, row.ClaimedAt, row.StartedAt, row.FinishedAt, row.CreatedAt, row.UpdatedAt)
}

func taskAttemptFromList(row gen.ListTaskAttemptsRow) domain.TaskAttempt {
	return taskAttemptFromFields(row.ID, row.PlanID, row.TaskID, row.AttemptNumber, row.State, row.SessionID, row.Harness, row.RuntimeRef, row.ClaimedAt, row.StartedAt, row.FinishedAt, row.CreatedAt, row.UpdatedAt)
}

func taskAttemptFromOpen(row gen.ListOpenTaskAttemptsRow) domain.TaskAttempt {
	return taskAttemptFromFields(row.ID, row.PlanID, row.TaskID, row.AttemptNumber, row.State, row.SessionID, row.Harness, row.RuntimeRef, row.ClaimedAt, row.StartedAt, row.FinishedAt, row.CreatedAt, row.UpdatedAt)
}

func taskAttemptFromFields(
	id, planID, taskID string,
	number int64,
	state domain.TaskAttemptState,
	sessionID *domain.SessionID,
	harness *domain.AgentHarness,
	runtimeRef string,
	claimedAt time.Time,
	startedAt, finishedAt sql.NullTime,
	createdAt, updatedAt time.Time,
) domain.TaskAttempt {
	attempt := domain.TaskAttempt{
		ID:            id,
		PlanID:        planID,
		TaskID:        taskID,
		AttemptNumber: int(number),
		State:         state,
		RuntimeRef:    runtimeRef,
		ClaimedAt:     claimedAt,
		CreatedAt:     createdAt,
		UpdatedAt:     updatedAt,
	}
	if sessionID != nil {
		attempt.SessionID = string(*sessionID)
	}
	if harness != nil {
		attempt.Harness = *harness
	}
	if startedAt.Valid {
		started := startedAt.Time
		attempt.StartedAt = &started
	}
	if finishedAt.Valid {
		finished := finishedAt.Time
		attempt.FinishedAt = &finished
	}
	return attempt
}

func taskResultFromRow(row gen.TaskResult) domain.TaskResult {
	return domain.TaskResult{
		ID:         row.ID,
		PlanID:     row.PlanID,
		TaskID:     row.TaskID,
		AttemptID:  row.AttemptID,
		Outcome:    row.Outcome,
		Summary:    row.Summary.String,
		Evidence:   []byte(row.Evidence),
		RecordedAt: row.RecordedAt,
	}
}

func nullableSessionID(id string) *domain.SessionID {
	if id == "" {
		return nil
	}
	sessionID := domain.SessionID(id)
	return &sessionID
}

func nullableHarness(harness domain.AgentHarness) *domain.AgentHarness {
	if harness == "" {
		return nil
	}
	value := harness
	return &value
}

func nullableTime(value *time.Time) sql.NullTime {
	if value == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: *value, Valid: true}
}
