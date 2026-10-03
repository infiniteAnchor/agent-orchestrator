package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/cdc"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// EnsurePlannerCursor records the change_log head the first time automation
// starts, so historical task results are not replayed as new work.
func (s *Store) EnsurePlannerCursor(ctx context.Context, now time.Time) error {
	if now.IsZero() {
		return errors.New("ensure planner cursor: time is required")
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "ensure planner cursor", func(q *gen.Queries) error {
		_, err := q.GetPlannerCursor(ctx, domain.PlannerConsumerTaskResults)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		seq, err := q.MaxChangeLogSeq(ctx)
		if err != nil {
			return err
		}
		return q.InsertPlannerCursor(ctx, gen.InsertPlannerCursorParams{
			Consumer: domain.PlannerConsumerTaskResults, LastSeq: seq, UpdatedAt: proposalTimestamp(now),
		})
	})
}

// PlannerCursor is the last task-result event automation has claimed.
func (s *Store) PlannerCursor(ctx context.Context) (int64, error) {
	row, err := s.qr.GetPlannerCursor(ctx, domain.PlannerConsumerTaskResults)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, domain.ErrPlannerCursorMissing
	}
	if err != nil {
		return 0, err
	}
	return row.LastSeq, nil
}

// AdvancePlannerCursor moves the cursor forward. A missing cursor is an error
// so a process cannot replay the log from zero by accident.
func (s *Store) AdvancePlannerCursor(ctx context.Context, seq int64, now time.Time) error {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "advance planner cursor", func(q *gen.Queries) error {
		if _, err := q.GetPlannerCursor(ctx, domain.PlannerConsumerTaskResults); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return domain.ErrPlannerCursorMissing
			}
			return err
		}
		_, err := q.AdvancePlannerCursor(ctx, gen.AdvancePlannerCursorParams{
			LastSeq: seq, UpdatedAt: proposalTimestamp(now),
			Consumer: domain.PlannerConsumerTaskResults, LastSeq_2: seq,
		})
		return err
	})
}

// ApplyResultEvent claims one task_result_recorded event. A repeated seq
// returns the existing follow-up and does not requeue or open another gate.
func (s *Store) ApplyResultEvent(ctx context.Context, ev cdc.Event, now time.Time, newID func() string) (domain.AppliedResultEvent, error) {
	if ev.Type != cdc.EventTaskResultRecorded || ev.Seq <= 0 || now.IsZero() || newID == nil {
		return domain.AppliedResultEvent{}, errors.New("apply result event: seq, type, time, and id are required")
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return domain.AppliedResultEvent{}, err
	}
	defer s.writeMu.Unlock()
	var applied domain.AppliedResultEvent
	err := s.inTx(ctx, "apply result event", func(q *gen.Queries) error {
		if _, err := q.GetPlannerCursor(ctx, domain.PlannerConsumerTaskResults); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return domain.ErrPlannerCursorMissing
			}
			return err
		}
		existing, err := q.GetPlannerFollowupBySeq(ctx, ev.Seq)
		if err == nil {
			applied.Followup = plannerFollowupFromGen(existing)
			applied.Decision, applied.Gate, err = loadRetrySideEffects(ctx, q, applied.Followup)
			if err != nil {
				return err
			}
			_, err = q.AdvancePlannerCursor(ctx, gen.AdvancePlannerCursorParams{
				LastSeq: ev.Seq, UpdatedAt: proposalTimestamp(now),
				Consumer: domain.PlannerConsumerTaskResults, LastSeq_2: ev.Seq,
			})
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var payload cdc.TaskGraphEventPayload
		if jsonErr := json.Unmarshal(ev.Payload, &payload); jsonErr != nil {
			return jsonErr
		}
		followup := domain.PlannerFollowup{
			ID: newID(), ProjectID: domain.ProjectID(ev.ProjectID), PlanID: payload.PlanID,
			TaskID: payload.TaskID, AttemptID: payload.AttemptID, SourceSeq: ev.Seq,
			State: domain.PlannerFollowupSkipped, CreatedAt: now, UpdatedAt: now,
		}
		if payload.PlanID == "" || payload.TaskID == "" || payload.AttemptID == "" || ev.ProjectID == "" {
			if err := insertFollowup(ctx, q, followup); err != nil {
				return err
			}
			applied.Followup = followup
			applied.Created = true
			_, err = q.AdvancePlannerCursor(ctx, gen.AdvancePlannerCursorParams{
				LastSeq: ev.Seq, UpdatedAt: proposalTimestamp(now),
				Consumer: domain.PlannerConsumerTaskResults, LastSeq_2: ev.Seq,
			})
			return err
		}
		decision, gate, state, err := settleAttempt(ctx, q, followup.ProjectID, payload.PlanID, payload.TaskID, payload.AttemptID, domain.RetryReasonFailed, now, newID, true)
		if err != nil {
			return err
		}
		followup.State = state
		if err := insertFollowup(ctx, q, followup); err != nil {
			return err
		}
		applied = domain.AppliedResultEvent{Followup: followup, Decision: decision, Gate: gate, Created: true}
		_, err = q.AdvancePlannerCursor(ctx, gen.AdvancePlannerCursorParams{
			LastSeq: ev.Seq, UpdatedAt: proposalTimestamp(now),
			Consumer: domain.PlannerConsumerTaskResults, LastSeq_2: ev.Seq,
		})
		return err
	})
	if err != nil {
		return domain.AppliedResultEvent{}, err
	}
	return applied, nil
}

// RetryAttempt applies retry policy to one attempt. The same attempt returns
// the original decision.
func (s *Store) RetryAttempt(ctx context.Context, projectID domain.ProjectID, planID, taskID, attemptID, reason string, now time.Time, newID func() string) (domain.RetryOutcome, error) {
	if now.IsZero() || newID == nil || attemptID == "" {
		return domain.RetryOutcome{}, errors.New("retry attempt: attempt, time, and id are required")
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return domain.RetryOutcome{}, err
	}
	defer s.writeMu.Unlock()
	var outcome domain.RetryOutcome
	err := s.inTx(ctx, "retry attempt", func(q *gen.Queries) error {
		owner, err := q.ActiveProjectForPlan(ctx, planID)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != projectID) {
			return domain.ErrTaskAutomationMissing
		}
		if err != nil {
			return err
		}
		if row, err := q.GetTaskRetryDecision(ctx, attemptID); err == nil {
			decision := retryDecisionFromGen(row)
			if decision.PlanID != planID || decision.TaskID != taskID {
				return domain.ErrTaskAutomationMissing
			}
			outcome.Decision = decision
			outcome.Repeated = true
			if decision.Action == domain.RetryActionEscalate {
				gate, err := pendingOrEscalationGate(ctx, q, projectID, attemptID)
				if err != nil {
					return err
				}
				outcome.Gate = gate
			}
			return nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		decision, gate, _, err := settleAttempt(ctx, q, projectID, planID, taskID, attemptID, reason, now, newID, false)
		if err != nil {
			return err
		}
		if decision == nil {
			return domain.ErrTaskRetryNotFailed
		}
		outcome.Decision = *decision
		outcome.Gate = gate
		switch decision.Action {
		case domain.RetryActionHold:
			return domain.ErrTaskRetryAmbiguous
		case domain.RetryActionNone:
			return domain.ErrTaskRetryNotFailed
		}
		return nil
	})
	if err != nil {
		return domain.RetryOutcome{}, err
	}
	return outcome, nil
}

func settleAttempt(
	ctx context.Context,
	q *gen.Queries,
	projectID domain.ProjectID,
	planID, taskID, attemptID, reason string,
	now time.Time,
	newID func() string,
	fromEvent bool,
) (*domain.TaskRetryDecision, *domain.HumanGate, domain.PlannerFollowupState, error) {
	attemptRow, err := q.GetTaskAttempt(ctx, attemptID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, domain.PlannerFollowupSkipped, nil
	}
	if err != nil {
		return nil, nil, "", err
	}
	attempt := taskAttemptFromGet(attemptRow)
	if attempt.PlanID != planID || attempt.TaskID != taskID {
		return nil, nil, domain.PlannerFollowupSkipped, nil
	}
	task, err := q.GetTask(ctx, gen.GetTaskParams{PlanID: planID, ID: taskID})
	if err != nil {
		return nil, nil, "", err
	}
	result, err := q.GetTaskResultByAttempt(ctx, attemptID)
	hasResult := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, "", err
	}
	count, err := q.CountTaskAttempts(ctx, gen.CountTaskAttemptsParams{PlanID: planID, TaskID: taskID})
	if err != nil {
		return nil, nil, "", err
	}
	policy := domain.TaskRetryPolicy{MaxAttempts: domain.DefaultMaxTaskAttempts}
	if row, err := q.GetTaskRetryPolicy(ctx, string(projectID)); err == nil {
		policy = domain.TaskRetryPolicy{MaxAttempts: int(row.MaxAttempts), FallbackHarness: row.FallbackHarness}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, "", err
	}
	input := domain.RetryInput{
		AttemptState: attempt.State, RuntimeRef: attempt.RuntimeRef, AttemptCount: int(count),
		MaxAttempts: policy.Normalized().MaxAttempts, CurrentHarness: task.Harness,
		FallbackHarness: policy.FallbackHarness, Reason: reason, HasResult: hasResult,
	}
	if hasResult {
		input.Outcome = result.Outcome
	}
	choice := domain.DecideRetry(input)
	state := domain.PlannerFollowupQueued
	if choice.Action == domain.RetryActionNone || choice.Action == domain.RetryActionHold {
		if choice.Action == domain.RetryActionHold {
			decision := domain.TaskRetryDecision{
				ID: newID(), ProjectID: projectID, PlanID: planID, TaskID: taskID, AttemptID: attemptID,
				Action: choice.Action, Reason: reason, CreatedAt: now,
			}
			if err := insertDecision(ctx, q, decision); err != nil {
				return nil, nil, "", err
			}
			return &decision, nil, state, nil
		}
		if fromEvent {
			return nil, nil, state, nil
		}
		return nil, nil, state, nil
	}
	if _, err := q.PendingTaskHumanGate(ctx, gen.PendingTaskHumanGateParams{PlanID: planID, TaskID: taskID}); err == nil && choice.Action.Requeues() {
		if fromEvent {
			return nil, nil, state, nil
		}
		return nil, nil, "", domain.ErrHumanGatePending
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, "", err
	}
	decision := domain.TaskRetryDecision{
		ID: newID(), ProjectID: projectID, PlanID: planID, TaskID: taskID, AttemptID: attemptID,
		Action: choice.Action, Harness: choice.Harness, Reason: reason, CreatedAt: now,
	}
	if err := insertDecision(ctx, q, decision); err != nil {
		return nil, nil, "", err
	}
	if choice.Action.Requeues() {
		rows, err := q.RequeueFailedTask(ctx, gen.RequeueFailedTaskParams{
			Harness: choice.Harness, UpdatedAt: now, PlanID: planID, ID: taskID,
		})
		if err != nil {
			return nil, nil, "", err
		}
		if rows == 0 {
			return nil, nil, "", fmt.Errorf("requeue task %s: task was not failed", taskID)
		}
		return &decision, nil, domain.PlannerFollowupQueued, nil
	}
	var gate *domain.HumanGate
	if choice.Action == domain.RetryActionEscalate {
		opened, err := openEscalationGate(ctx, q, projectID, planID, taskID, attemptID, resultSummary(result, hasResult), now, newID)
		if err != nil {
			return nil, nil, "", err
		}
		gate = &opened
	}
	return &decision, gate, domain.PlannerFollowupQueued, nil
}

func resultSummary(result gen.TaskResult, ok bool) string {
	if !ok || !result.Summary.Valid || result.Summary.String == "" {
		return "Task retries are exhausted"
	}
	return domain.TruncateHandoffSummary(result.Summary.String)
}

func openEscalationGate(ctx context.Context, q *gen.Queries, projectID domain.ProjectID, planID, taskID, attemptID, summary string, now time.Time, newID func() string) (domain.HumanGate, error) {
	key := "escalate/" + attemptID
	if row, err := q.GetTaskHumanGateByRequest(ctx, gen.GetTaskHumanGateByRequestParams{ProjectID: string(projectID), RequestKey: key}); err == nil {
		return humanGateFromGen(row), nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return domain.HumanGate{}, err
	}
	if summary == "" {
		summary = "Task retries are exhausted"
	}
	summary = domain.TruncateHandoffSummary(summary)
	gate := domain.HumanGate{
		ID: newID(), ProjectID: projectID, PlanID: planID, TaskID: taskID, AttemptID: attemptID,
		RequestKey: key, State: domain.HumanGatePending, Summary: summary, CreatedAt: now, UpdatedAt: now,
	}
	err := q.InsertTaskHumanGate(ctx, gen.InsertTaskHumanGateParams{
		ID: gate.ID, ProjectID: string(gate.ProjectID), PlanID: gate.PlanID, TaskID: gate.TaskID,
		AttemptID: gate.AttemptID, RequestKey: gate.RequestKey, State: string(gate.State), Summary: gate.Summary,
		CreatedAt: proposalTimestamp(gate.CreatedAt), UpdatedAt: proposalTimestamp(gate.UpdatedAt),
	})
	if err != nil && isSQLiteUnique(err) {
		row, getErr := q.GetTaskHumanGateByRequest(ctx, gen.GetTaskHumanGateByRequestParams{ProjectID: string(projectID), RequestKey: key})
		if getErr != nil {
			return domain.HumanGate{}, getErr
		}
		return humanGateFromGen(row), nil
	}
	return gate, err
}

func insertFollowup(ctx context.Context, q *gen.Queries, followup domain.PlannerFollowup) error {
	return q.InsertPlannerFollowup(ctx, gen.InsertPlannerFollowupParams{
		ID: followup.ID, ProjectID: string(followup.ProjectID), PlanID: followup.PlanID,
		TaskID: followup.TaskID, AttemptID: followup.AttemptID, SourceSeq: followup.SourceSeq,
		State: string(followup.State), TurnID: followup.TurnID, OrchestratorID: string(followup.OrchestratorID),
		ErrorCode: followup.ErrorCode, Summary: followup.Summary,
		CreatedAt: proposalTimestamp(followup.CreatedAt), UpdatedAt: proposalTimestamp(followup.UpdatedAt),
	})
}

func insertDecision(ctx context.Context, q *gen.Queries, decision domain.TaskRetryDecision) error {
	return q.InsertTaskRetryDecision(ctx, gen.InsertTaskRetryDecisionParams{
		ID: decision.ID, ProjectID: string(decision.ProjectID), PlanID: decision.PlanID,
		TaskID: decision.TaskID, AttemptID: decision.AttemptID, Action: string(decision.Action),
		Harness: decision.Harness, Reason: decision.Reason, CreatedAt: proposalTimestamp(decision.CreatedAt),
	})
}

func loadRetrySideEffects(ctx context.Context, q *gen.Queries, followup domain.PlannerFollowup) (*domain.TaskRetryDecision, *domain.HumanGate, error) {
	if followup.AttemptID == "" {
		return nil, nil, nil
	}
	var decision *domain.TaskRetryDecision
	row, err := q.GetTaskRetryDecision(ctx, followup.AttemptID)
	if err == nil {
		decoded := retryDecisionFromGen(row)
		decision = &decoded
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, err
	}
	gate, err := pendingOrEscalationGate(ctx, q, followup.ProjectID, followup.AttemptID)
	if err != nil {
		return nil, nil, err
	}
	return decision, gate, nil
}

func pendingOrEscalationGate(ctx context.Context, q *gen.Queries, projectID domain.ProjectID, attemptID string) (*domain.HumanGate, error) {
	row, err := q.GetTaskHumanGateByRequest(ctx, gen.GetTaskHumanGateByRequestParams{
		ProjectID: string(projectID), RequestKey: "escalate/" + attemptID,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	gate := humanGateFromGen(row)
	return &gate, nil
}

// ListPendingPlannerFollowups returns reviewer turns that still need to run.
func (s *Store) ListPendingPlannerFollowups(ctx context.Context) ([]domain.PlannerFollowup, error) {
	rows, err := s.qr.ListPendingPlannerFollowups(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.PlannerFollowup, 0, len(rows))
	for _, row := range rows {
		out = append(out, plannerFollowupFromGen(row))
	}
	return out, nil
}

// SetPlannerFollowupState compare-and-swaps one follow-up.
func (s *Store) SetPlannerFollowupState(ctx context.Context, followup domain.PlannerFollowup, from domain.PlannerFollowupState, now time.Time) (bool, error) {
	if now.IsZero() || followup.ID == "" {
		return false, errors.New("set planner follow-up: id and time are required")
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return false, err
	}
	defer s.writeMu.Unlock()
	rows, err := s.qw.SetPlannerFollowupState(ctx, gen.SetPlannerFollowupStateParams{
		State: string(followup.State), TurnID: followup.TurnID, OrchestratorID: string(followup.OrchestratorID),
		ErrorCode: followup.ErrorCode, Summary: domain.TruncateHandoffSummary(followup.Summary),
		UpdatedAt: proposalTimestamp(now), ID: followup.ID, State_2: string(from),
	})
	return rows > 0, err
}

// InsertTaskHandoff stores one bounded summary for an attempt.
func (s *Store) InsertTaskHandoff(ctx context.Context, handoff domain.TaskHandoff) error {
	if err := domain.ValidateHandoffSummary(handoff.Summary); err != nil {
		return err
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	return s.qw.InsertTaskHandoff(ctx, gen.InsertTaskHandoffParams{
		ID: handoff.ID, ProjectID: string(handoff.ProjectID), PlanID: handoff.PlanID,
		TaskID: handoff.TaskID, AttemptID: handoff.AttemptID, Summary: handoff.Summary,
		CreatedAt: proposalTimestamp(handoff.CreatedAt),
	})
}

// ListTaskHandoffs returns summaries for one task in creation order.
func (s *Store) ListTaskHandoffs(ctx context.Context, planID, taskID string) ([]domain.TaskHandoff, error) {
	rows, err := s.qr.ListTaskHandoffs(ctx, gen.ListTaskHandoffsParams{PlanID: planID, TaskID: taskID})
	if err != nil {
		return nil, err
	}
	out := make([]domain.TaskHandoff, 0, len(rows))
	for _, row := range rows {
		created, err := time.Parse(proposalTimestampLayout, row.CreatedAt)
		if err != nil {
			return nil, err
		}
		out = append(out, domain.TaskHandoff{
			ID: row.ID, ProjectID: domain.ProjectID(row.ProjectID), PlanID: row.PlanID,
			TaskID: row.TaskID, AttemptID: row.AttemptID, Summary: row.Summary, CreatedAt: created,
		})
	}
	return out, nil
}

// GetTaskRetryPolicy returns the stored policy or the default when unset.
func (s *Store) GetTaskRetryPolicy(ctx context.Context, projectID domain.ProjectID) (domain.TaskRetryPolicy, error) {
	row, err := s.qr.GetTaskRetryPolicy(ctx, string(projectID))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.TaskRetryPolicy{ProjectID: projectID, MaxAttempts: domain.DefaultMaxTaskAttempts}.Normalized(), nil
	}
	if err != nil {
		return domain.TaskRetryPolicy{}, err
	}
	updated, err := time.Parse(proposalTimestampLayout, row.UpdatedAt)
	if err != nil {
		return domain.TaskRetryPolicy{}, err
	}
	return domain.TaskRetryPolicy{
		ProjectID: projectID, MaxAttempts: int(row.MaxAttempts), FallbackHarness: row.FallbackHarness, UpdatedAt: updated,
	}.Normalized(), nil
}

// UpsertTaskRetryPolicy replaces the project policy.
func (s *Store) UpsertTaskRetryPolicy(ctx context.Context, policy domain.TaskRetryPolicy) error {
	policy = policy.Normalized()
	if policy.UpdatedAt.IsZero() || policy.ProjectID == "" {
		return errors.New("upsert retry policy: project and time are required")
	}
	if policy.FallbackHarness != "" && !domain.AgentHarness(policy.FallbackHarness).IsKnown() {
		return errors.New("upsert retry policy: fallback harness is unknown")
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	return s.qw.UpsertTaskRetryPolicy(ctx, gen.UpsertTaskRetryPolicyParams{
		ProjectID: string(policy.ProjectID), MaxAttempts: int64(policy.MaxAttempts),
		FallbackHarness: policy.FallbackHarness, UpdatedAt: proposalTimestamp(policy.UpdatedAt),
	})
}

// OpenHumanGate inserts one pending gate. The same request key returns the
// original gate.
func (s *Store) OpenHumanGate(ctx context.Context, gate domain.HumanGate) (domain.HumanGate, bool, error) {
	if err := domain.ValidateHandoffSummary(gate.Summary); err != nil {
		return domain.HumanGate{}, false, err
	}
	if gate.ID == "" || gate.RequestKey == "" || gate.CreatedAt.IsZero() {
		return domain.HumanGate{}, false, errors.New("open human gate: id, request key, and time are required")
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return domain.HumanGate{}, false, err
	}
	defer s.writeMu.Unlock()
	err := s.inTx(ctx, "open human gate", func(q *gen.Queries) error {
		owner, err := q.ActiveProjectForPlan(ctx, gate.PlanID)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != gate.ProjectID) {
			return domain.ErrTaskAutomationMissing
		}
		if err != nil {
			return err
		}
		if _, err := q.GetTask(ctx, gen.GetTaskParams{PlanID: gate.PlanID, ID: gate.TaskID}); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return domain.ErrTaskAutomationMissing
			}
			return err
		}
		return q.InsertTaskHumanGate(ctx, gen.InsertTaskHumanGateParams{
			ID: gate.ID, ProjectID: string(gate.ProjectID), PlanID: gate.PlanID, TaskID: gate.TaskID,
			AttemptID: gate.AttemptID, RequestKey: gate.RequestKey, State: string(domain.HumanGatePending),
			Summary: gate.Summary, CreatedAt: proposalTimestamp(gate.CreatedAt), UpdatedAt: proposalTimestamp(gate.CreatedAt),
		})
	})
	if err != nil && !isSQLiteUnique(err) {
		return domain.HumanGate{}, false, err
	}
	got, ok, getErr := s.GetHumanGateByRequest(ctx, gate.ProjectID, gate.RequestKey)
	if getErr != nil {
		return domain.HumanGate{}, false, getErr
	}
	if !ok {
		return domain.HumanGate{}, false, domain.ErrHumanGateExists
	}
	if got.Summary != gate.Summary || got.TaskID != gate.TaskID || got.PlanID != gate.PlanID {
		return domain.HumanGate{}, false, domain.ErrHumanGateKeyReused
	}
	return got, got.ID == gate.ID, nil
}

// GetHumanGate loads one gate in a project.
func (s *Store) GetHumanGate(ctx context.Context, projectID domain.ProjectID, id string) (domain.HumanGate, bool, error) {
	row, err := s.qr.GetTaskHumanGate(ctx, gen.GetTaskHumanGateParams{ProjectID: string(projectID), ID: id})
	if errors.Is(err, sql.ErrNoRows) {
		return domain.HumanGate{}, false, nil
	}
	if err != nil {
		return domain.HumanGate{}, false, err
	}
	return humanGateFromGen(row), true, nil
}

// GetHumanGateByRequest loads the gate stored for one idempotency key.
func (s *Store) GetHumanGateByRequest(ctx context.Context, projectID domain.ProjectID, requestKey string) (domain.HumanGate, bool, error) {
	row, err := s.qr.GetTaskHumanGateByRequest(ctx, gen.GetTaskHumanGateByRequestParams{ProjectID: string(projectID), RequestKey: requestKey})
	if errors.Is(err, sql.ErrNoRows) {
		return domain.HumanGate{}, false, nil
	}
	if err != nil {
		return domain.HumanGate{}, false, err
	}
	return humanGateFromGen(row), true, nil
}

// ListHumanGates lists gates for one plan.
func (s *Store) ListHumanGates(ctx context.Context, projectID domain.ProjectID, planID string) ([]domain.HumanGate, error) {
	rows, err := s.qr.ListTaskHumanGates(ctx, gen.ListTaskHumanGatesParams{ProjectID: string(projectID), PlanID: planID})
	if err != nil {
		return nil, err
	}
	out := make([]domain.HumanGate, 0, len(rows))
	for _, row := range rows {
		out = append(out, humanGateFromGen(row))
	}
	return out, nil
}

// ResolveHumanGate approves or rejects a pending gate. Rejection cancels a
// task that is still queued. Approval does not dispatch.
func (s *Store) ResolveHumanGate(ctx context.Context, projectID domain.ProjectID, id string, to domain.HumanGateState, now time.Time) (domain.HumanGate, error) {
	if now.IsZero() || (to != domain.HumanGateApproved && to != domain.HumanGateRejected) {
		return domain.HumanGate{}, errors.New("resolve human gate: time and terminal state are required")
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return domain.HumanGate{}, err
	}
	defer s.writeMu.Unlock()
	err := s.inTx(ctx, "resolve human gate", func(q *gen.Queries) error {
		row, err := q.GetTaskHumanGate(ctx, gen.GetTaskHumanGateParams{ProjectID: string(projectID), ID: id})
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrTaskAutomationMissing
		}
		if err != nil {
			return err
		}
		gate := humanGateFromGen(row)
		if gate.State == to {
			return nil
		}
		if gate.State != domain.HumanGatePending {
			return domain.ErrHumanGateNotPending
		}
		rows, err := q.ResolveTaskHumanGate(ctx, gen.ResolveTaskHumanGateParams{
			State: string(to), UpdatedAt: proposalTimestamp(now),
			ResolvedAt: sql.NullString{String: proposalTimestamp(now), Valid: true},
			ID:         id, ProjectID: string(projectID),
		})
		if err != nil {
			return err
		}
		if rows == 0 {
			return domain.ErrHumanGateNotPending
		}
		if to != domain.HumanGateRejected {
			return nil
		}
		task, err := q.GetTask(ctx, gen.GetTaskParams{PlanID: gate.PlanID, ID: gate.TaskID})
		if err != nil {
			return err
		}
		if task.State != domain.TaskStateQueued {
			return nil
		}
		changed, err := q.TransitionTaskState(ctx, gen.TransitionTaskStateParams{
			State: domain.TaskStateCancelled, UpdatedAt: now, PlanID: gate.PlanID, ID: gate.TaskID, State_2: domain.TaskStateQueued,
		})
		if err != nil {
			return err
		}
		if changed == 0 {
			return domain.ErrHumanGateNotPending
		}
		return nil
	})
	if err != nil {
		return domain.HumanGate{}, err
	}
	gate, ok, err := s.GetHumanGate(ctx, projectID, id)
	if err != nil {
		return domain.HumanGate{}, err
	}
	if !ok {
		return domain.HumanGate{}, domain.ErrTaskAutomationMissing
	}
	return gate, nil
}

func plannerFollowupFromGen(row gen.PlannerFollowup) domain.PlannerFollowup {
	created, _ := time.Parse(proposalTimestampLayout, row.CreatedAt)
	updated, _ := time.Parse(proposalTimestampLayout, row.UpdatedAt)
	return domain.PlannerFollowup{
		ID: row.ID, ProjectID: domain.ProjectID(row.ProjectID), PlanID: row.PlanID, TaskID: row.TaskID,
		AttemptID: row.AttemptID, SourceSeq: row.SourceSeq, State: domain.PlannerFollowupState(row.State),
		TurnID: row.TurnID, OrchestratorID: domain.SessionID(row.OrchestratorID), ErrorCode: row.ErrorCode,
		Summary: row.Summary, CreatedAt: created, UpdatedAt: updated,
	}
}

func retryDecisionFromGen(row gen.TaskRetryDecision) domain.TaskRetryDecision {
	created, _ := time.Parse(proposalTimestampLayout, row.CreatedAt)
	return domain.TaskRetryDecision{
		ID: row.ID, ProjectID: domain.ProjectID(row.ProjectID), PlanID: row.PlanID, TaskID: row.TaskID,
		AttemptID: row.AttemptID, Action: domain.RetryAction(row.Action), Harness: row.Harness,
		Reason: row.Reason, CreatedAt: created,
	}
}

func humanGateFromGen(row gen.TaskHumanGate) domain.HumanGate {
	created, _ := time.Parse(proposalTimestampLayout, row.CreatedAt)
	updated, _ := time.Parse(proposalTimestampLayout, row.UpdatedAt)
	gate := domain.HumanGate{
		ID: row.ID, ProjectID: domain.ProjectID(row.ProjectID), PlanID: row.PlanID, TaskID: row.TaskID,
		AttemptID: row.AttemptID, RequestKey: row.RequestKey, State: domain.HumanGateState(row.State),
		Summary: row.Summary, CreatedAt: created, UpdatedAt: updated,
	}
	if row.ResolvedAt.Valid {
		if resolved, err := time.Parse(proposalTimestampLayout, row.ResolvedAt.String); err == nil {
			gate.ResolvedAt = &resolved
		}
	}
	return gate
}
