// Package taskauto consumes durable task-result events, records one reviewer
// turn per event, and applies retry, fallback, and human-gate policy.
package taskauto

import (
	"context"
	"errors"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/cdc"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

const (
	maxRequestKeyBytes = 128
	consumeBatch       = 50
	defaultInterval    = 2 * time.Second
)

// Store is the durable automation boundary.
type Store interface {
	GetProject(context.Context, string) (domain.ProjectRecord, bool, error)
	EnsurePlannerCursor(context.Context, time.Time) error
	PlannerCursor(context.Context) (int64, error)
	AdvancePlannerCursor(context.Context, int64, time.Time) error
	EventsAfter(context.Context, int64, int) ([]cdc.Event, error)
	ApplyResultEvent(context.Context, cdc.Event, time.Time, func() string) (domain.AppliedResultEvent, error)
	ListPendingPlannerFollowups(context.Context) ([]domain.PlannerFollowup, error)
	SetPlannerFollowupState(context.Context, domain.PlannerFollowup, domain.PlannerFollowupState, time.Time) (bool, error)
	InsertTaskHandoff(context.Context, domain.TaskHandoff) error
	ListTaskHandoffs(context.Context, string, string) ([]domain.TaskHandoff, error)
	GetTaskRetryPolicy(context.Context, domain.ProjectID) (domain.TaskRetryPolicy, error)
	UpsertTaskRetryPolicy(context.Context, domain.TaskRetryPolicy) error
	RetryAttempt(context.Context, domain.ProjectID, string, string, string, string, time.Time, func() string) (domain.RetryOutcome, error)
	OpenHumanGate(context.Context, domain.HumanGate) (domain.HumanGate, bool, error)
	GetHumanGate(context.Context, domain.ProjectID, string) (domain.HumanGate, bool, error)
	ListHumanGates(context.Context, domain.ProjectID, string) ([]domain.HumanGate, error)
	ResolveHumanGate(context.Context, domain.ProjectID, string, domain.HumanGateState, time.Time) (domain.HumanGate, error)
	LoadTaskSchedule(context.Context, domain.ProjectID, string) (domain.TaskScheduleView, bool, error)
}

// Reviewer runs one idempotent planner/reviewer turn for a follow-up.
type Reviewer interface {
	Review(context.Context, domain.PlannerFollowup) (string, error)
}

// Manager is the authenticated project API.
type Manager interface {
	PutPolicy(context.Context, domain.ProjectID, domain.TaskRetryPolicy) (domain.TaskRetryPolicy, error)
	GetPolicy(context.Context, domain.ProjectID) (domain.TaskRetryPolicy, error)
	Retry(context.Context, domain.ProjectID, string, string, RetryInput) (domain.RetryOutcome, error)
	RecordHandoff(context.Context, domain.ProjectID, string, string, HandoffInput) (domain.TaskHandoff, error)
	ListHandoffs(context.Context, domain.ProjectID, string, string) ([]domain.TaskHandoff, error)
	OpenGate(context.Context, domain.ProjectID, string, string, GateInput) (domain.HumanGate, error)
	ListGates(context.Context, domain.ProjectID, string) ([]domain.HumanGate, error)
	ResolveGate(context.Context, domain.ProjectID, string, string, domain.HumanGateState) (domain.HumanGate, error)
}

// RetryInput identifies the attempt and why it should move.
type RetryInput struct {
	AttemptID string `json:"attemptId" maxLength:"128"`
	Reason    string `json:"reason" enum:"failed,provider_exhausted"`
}

// HandoffInput is a bounded task-linked summary.
type HandoffInput struct {
	AttemptID string `json:"attemptId" maxLength:"128"`
	Summary   string `json:"summary" maxLength:"4096"`
}

// GateInput opens one human gate. RequestKey makes the create retry-safe.
type GateInput struct {
	RequestKey string `json:"requestKey" maxLength:"128"`
	AttemptID  string `json:"attemptId,omitempty" maxLength:"128"`
	Summary    string `json:"summary" maxLength:"4096"`
}

// PolicyInput is the project retry configuration.
type PolicyInput struct {
	MaxAttempts     int    `json:"maxAttempts" minimum:"1" maximum:"8"`
	FallbackHarness string `json:"fallbackHarness,omitempty" maxLength:"64"`
}

// Service applies Phase 4 policy from durable task results.
type Service struct {
	store      Store
	reviewer   Reviewer
	now        func() time.Time
	newID      func() string
	interval   time.Duration
	syncReview bool

	mu      sync.Mutex
	rootCtx context.Context
	running map[string]struct{}
}

// New constructs the automation service. A nil reviewer records follow-ups
// without starting a Chat turn.
func New(store Store, reviewer Reviewer) *Service {
	return &Service{
		store: store, reviewer: reviewer, now: func() time.Time { return time.Now().UTC() },
		newID: uuid.NewString, interval: defaultInterval, running: map[string]struct{}{},
	}
}

// Run initializes the cursor at the log head, resumes queued turns, and then
// consumes new task-result events until ctx is cancelled. Call it only after
// task recovery has finished.
func (s *Service) Run(ctx context.Context) error {
	if s == nil || s.store == nil {
		return nil
	}
	s.mu.Lock()
	s.rootCtx = ctx
	s.mu.Unlock()
	if err := s.store.EnsurePlannerCursor(ctx, s.now()); err != nil {
		return err
	}
	if err := s.resume(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := s.Consume(ctx); err != nil && ctx.Err() == nil {
				return err
			}
		}
	}
}

// Consume claims the next batch of change_log events. Task results get one
// follow-up; every other event only advances the cursor.
func (s *Service) Consume(ctx context.Context) error {
	cursor, err := s.store.PlannerCursor(ctx)
	if err != nil {
		return err
	}
	events, err := s.store.EventsAfter(ctx, cursor, consumeBatch)
	if err != nil {
		return err
	}
	for _, ev := range events {
		if ev.Type != cdc.EventTaskResultRecorded {
			if err := s.store.AdvancePlannerCursor(ctx, ev.Seq, s.now()); err != nil {
				return err
			}
			continue
		}
		applied, err := s.store.ApplyResultEvent(ctx, ev, s.now(), s.newID)
		if err != nil {
			return err
		}
		if applied.Created && applied.Followup.State == domain.PlannerFollowupQueued {
			s.start(ctx, applied.Followup)
		}
	}
	return nil
}

func (s *Service) resume(ctx context.Context) error {
	pending, err := s.store.ListPendingPlannerFollowups(ctx)
	if err != nil {
		return err
	}
	for _, followup := range pending {
		s.start(ctx, followup)
	}
	return nil
}

func (s *Service) start(ctx context.Context, followup domain.PlannerFollowup) {
	if s == nil || s.reviewer == nil {
		return
	}
	if followup.State != domain.PlannerFollowupQueued && followup.State != domain.PlannerFollowupRunning {
		return
	}
	s.mu.Lock()
	if _, ok := s.running[followup.ID]; ok {
		s.mu.Unlock()
		return
	}
	if s.rootCtx != nil {
		ctx = s.rootCtx
	}
	s.running[followup.ID] = struct{}{}
	sync := s.syncReview
	s.mu.Unlock()
	run := func() {
		defer func() {
			s.mu.Lock()
			delete(s.running, followup.ID)
			s.mu.Unlock()
		}()
		s.review(ctx, followup)
	}
	if sync {
		run()
		return
	}
	go run()
}

func (s *Service) review(ctx context.Context, followup domain.PlannerFollowup) {
	if followup.State == domain.PlannerFollowupQueued {
		followup.State = domain.PlannerFollowupRunning
		changed, err := s.store.SetPlannerFollowupState(ctx, followup, domain.PlannerFollowupQueued, s.now())
		if err != nil || !changed {
			return
		}
	}
	summary, err := s.reviewer.Review(ctx, followup)
	if err != nil {
		followup.State = domain.PlannerFollowupFailed
		followup.ErrorCode = "REVIEW_FAILED"
		_, _ = s.store.SetPlannerFollowupState(ctx, followup, domain.PlannerFollowupRunning, s.now())
		return
	}
	summary = domain.TruncateHandoffSummary(summary)
	if summary != "" {
		_ = s.store.InsertTaskHandoff(ctx, domain.TaskHandoff{
			ID: s.newID(), ProjectID: followup.ProjectID, PlanID: followup.PlanID,
			TaskID: followup.TaskID, AttemptID: followup.AttemptID, Summary: summary, CreatedAt: s.now(),
		})
	}
	followup.State = domain.PlannerFollowupCompleted
	followup.Summary = summary
	followup.ErrorCode = ""
	_, _ = s.store.SetPlannerFollowupState(ctx, followup, domain.PlannerFollowupRunning, s.now())
}

// PutPolicy stores the project retry and fallback configuration.
func (s *Service) PutPolicy(ctx context.Context, projectID domain.ProjectID, policy domain.TaskRetryPolicy) (domain.TaskRetryPolicy, error) {
	if err := s.requireProject(ctx, projectID); err != nil {
		return domain.TaskRetryPolicy{}, err
	}
	if policy.MaxAttempts < 1 || policy.MaxAttempts > domain.MaxTaskAttemptsCap {
		return domain.TaskRetryPolicy{}, apierr.Invalid("INVALID_RETRY_POLICY", "maxAttempts must be from 1 to 8", nil)
	}
	if policy.FallbackHarness != "" && !domain.AgentHarness(policy.FallbackHarness).IsKnown() {
		return domain.TaskRetryPolicy{}, apierr.Invalid("INVALID_FALLBACK_HARNESS", "fallbackHarness is not a known harness", nil)
	}
	policy.ProjectID = projectID
	policy.UpdatedAt = s.now()
	if err := s.store.UpsertTaskRetryPolicy(ctx, policy); err != nil {
		return domain.TaskRetryPolicy{}, apierr.Internal("TASK_RETRY_POLICY_FAILED", "Failed to store the retry policy")
	}
	return s.store.GetTaskRetryPolicy(ctx, projectID)
}

// GetPolicy returns the stored policy or the default.
func (s *Service) GetPolicy(ctx context.Context, projectID domain.ProjectID) (domain.TaskRetryPolicy, error) {
	if err := s.requireProject(ctx, projectID); err != nil {
		return domain.TaskRetryPolicy{}, err
	}
	policy, err := s.store.GetTaskRetryPolicy(ctx, projectID)
	if err != nil {
		return domain.TaskRetryPolicy{}, apierr.Internal("TASK_RETRY_POLICY_FAILED", "Failed to load the retry policy")
	}
	return policy, nil
}

// Retry applies the durable policy to one attempt. A repeated call returns the
// original decision. Approval does not dispatch, and an ambiguous attempt is
// not requeued.
func (s *Service) Retry(ctx context.Context, projectID domain.ProjectID, planID, taskID string, in RetryInput) (domain.RetryOutcome, error) {
	if err := s.requireProject(ctx, projectID); err != nil {
		return domain.RetryOutcome{}, err
	}
	if in.AttemptID == "" || len(in.AttemptID) > maxRequestKeyBytes {
		return domain.RetryOutcome{}, apierr.Invalid("INVALID_ATTEMPT_ID", "attemptId is required and must be at most 128 bytes", nil)
	}
	if in.Reason != domain.RetryReasonFailed && in.Reason != domain.RetryReasonProviderExhausted {
		return domain.RetryOutcome{}, apierr.Invalid("INVALID_RETRY_REASON", "reason must be failed or provider_exhausted", nil)
	}
	outcome, err := s.store.RetryAttempt(ctx, projectID, planID, taskID, in.AttemptID, in.Reason, s.now(), s.newID)
	if err != nil {
		return domain.RetryOutcome{}, retryErr(err)
	}
	return outcome, nil
}

// RecordHandoff stores a bounded summary linked to a task attempt.
func (s *Service) RecordHandoff(ctx context.Context, projectID domain.ProjectID, planID, taskID string, in HandoffInput) (domain.TaskHandoff, error) {
	if err := s.requireTask(ctx, projectID, planID, taskID); err != nil {
		return domain.TaskHandoff{}, err
	}
	if err := domain.ValidateHandoffSummary(in.Summary); err != nil {
		return domain.TaskHandoff{}, apierr.Invalid("INVALID_HANDOFF", "summary must be nonempty and at most 4096 bytes", nil)
	}
	if in.AttemptID == "" || len(in.AttemptID) > maxRequestKeyBytes {
		return domain.TaskHandoff{}, apierr.Invalid("INVALID_ATTEMPT_ID", "attemptId is required and must be at most 128 bytes", nil)
	}
	handoff := domain.TaskHandoff{
		ID: s.newID(), ProjectID: projectID, PlanID: planID, TaskID: taskID,
		AttemptID: in.AttemptID, Summary: in.Summary, CreatedAt: s.now(),
	}
	if err := s.store.InsertTaskHandoff(ctx, handoff); err != nil {
		return domain.TaskHandoff{}, apierr.Internal("TASK_HANDOFF_FAILED", "Failed to store the handoff summary")
	}
	return handoff, nil
}

// ListHandoffs returns the summaries stored for one task.
func (s *Service) ListHandoffs(ctx context.Context, projectID domain.ProjectID, planID, taskID string) ([]domain.TaskHandoff, error) {
	if err := s.requireTask(ctx, projectID, planID, taskID); err != nil {
		return nil, err
	}
	rows, err := s.store.ListTaskHandoffs(ctx, planID, taskID)
	if err != nil {
		return nil, apierr.Internal("TASK_HANDOFF_FAILED", "Failed to list handoff summaries")
	}
	return rows, nil
}

// OpenGate records work that needs a person. The same request key returns the
// original gate. A pending gate blocks claiming that task.
func (s *Service) OpenGate(ctx context.Context, projectID domain.ProjectID, planID, taskID string, in GateInput) (domain.HumanGate, error) {
	if err := s.requireTask(ctx, projectID, planID, taskID); err != nil {
		return domain.HumanGate{}, err
	}
	if in.RequestKey == "" || len(in.RequestKey) > maxRequestKeyBytes || !utf8.ValidString(in.RequestKey) {
		return domain.HumanGate{}, apierr.Invalid("INVALID_REQUEST_KEY", "requestKey must be nonempty and at most 128 bytes", nil)
	}
	if err := domain.ValidateHandoffSummary(in.Summary); err != nil {
		return domain.HumanGate{}, apierr.Invalid("INVALID_GATE_SUMMARY", "summary must be nonempty and at most 4096 bytes", nil)
	}
	gate, _, err := s.store.OpenHumanGate(ctx, domain.HumanGate{
		ID: s.newID(), ProjectID: projectID, PlanID: planID, TaskID: taskID,
		AttemptID: in.AttemptID, RequestKey: in.RequestKey, Summary: in.Summary, CreatedAt: s.now(),
	})
	if err != nil {
		return domain.HumanGate{}, gateErr(err)
	}
	return gate, nil
}

// ListGates returns the gates stored for one plan.
func (s *Service) ListGates(ctx context.Context, projectID domain.ProjectID, planID string) ([]domain.HumanGate, error) {
	if err := s.requirePlan(ctx, projectID, planID); err != nil {
		return nil, err
	}
	rows, err := s.store.ListHumanGates(ctx, projectID, planID)
	if err != nil {
		return nil, apierr.Internal("TASK_GATE_FAILED", "Failed to list human gates")
	}
	return rows, nil
}

// ResolveGate approves or rejects a gate. Approval does not dispatch. Rejecting
// a still-queued task cancels it.
func (s *Service) ResolveGate(ctx context.Context, projectID domain.ProjectID, planID, gateID string, to domain.HumanGateState) (domain.HumanGate, error) {
	if err := s.requirePlan(ctx, projectID, planID); err != nil {
		return domain.HumanGate{}, err
	}
	gate, err := s.store.ResolveHumanGate(ctx, projectID, gateID, to, s.now())
	if err != nil {
		return domain.HumanGate{}, gateErr(err)
	}
	if gate.PlanID != planID {
		return domain.HumanGate{}, apierr.NotFound("TASK_GATE_NOT_FOUND", "Unknown human gate")
	}
	return gate, nil
}

func (s *Service) requireProject(ctx context.Context, projectID domain.ProjectID) error {
	if s == nil || s.store == nil {
		return apierr.Internal("TASK_AUTOMATION_UNAVAILABLE", "Task automation is unavailable")
	}
	record, ok, err := s.store.GetProject(ctx, string(projectID))
	if err != nil {
		return apierr.Internal("PROJECT_LOAD_FAILED", "Failed to load project")
	}
	if !ok || !record.ArchivedAt.IsZero() {
		return apierr.NotFound("PROJECT_NOT_FOUND", "Unknown project")
	}
	return nil
}

func (s *Service) requirePlan(ctx context.Context, projectID domain.ProjectID, planID string) error {
	if err := s.requireProject(ctx, projectID); err != nil {
		return err
	}
	_, ok, err := s.store.LoadTaskSchedule(ctx, projectID, planID)
	if err != nil {
		return apierr.Internal("TASK_PLAN_LOAD_FAILED", "Failed to load task plan")
	}
	if !ok {
		return apierr.NotFound("TASK_PLAN_NOT_FOUND", "Unknown task plan")
	}
	return nil
}

func (s *Service) requireTask(ctx context.Context, projectID domain.ProjectID, planID, taskID string) error {
	if err := s.requirePlan(ctx, projectID, planID); err != nil {
		return err
	}
	view, ok, err := s.store.LoadTaskSchedule(ctx, projectID, planID)
	if err != nil || !ok {
		return apierr.NotFound("TASK_PLAN_NOT_FOUND", "Unknown task plan")
	}
	for _, node := range view.Nodes {
		if node.ID == taskID {
			return nil
		}
	}
	return apierr.NotFound("TASK_NOT_FOUND", "Unknown task")
}

func retryErr(err error) error {
	switch {
	case errors.Is(err, domain.ErrTaskAutomationMissing):
		return apierr.NotFound("TASK_ATTEMPT_NOT_FOUND", "Unknown task attempt")
	case errors.Is(err, domain.ErrTaskRetryAmbiguous):
		return apierr.Conflict("TASK_RETRY_AMBIGUOUS", "An ambiguous attempt cannot be retried", nil)
	case errors.Is(err, domain.ErrTaskRetryNotFailed):
		return apierr.Conflict("TASK_RETRY_NOT_FAILED", "This attempt is not a retryable failure", nil)
	case errors.Is(err, domain.ErrHumanGatePending):
		return apierr.Conflict("HUMAN_GATE_PENDING", "Resolve the pending human gate before retrying", nil)
	default:
		return apierr.Internal("TASK_RETRY_FAILED", "Failed to apply the retry policy")
	}
}

func gateErr(err error) error {
	switch {
	case errors.Is(err, domain.ErrTaskAutomationMissing):
		return apierr.NotFound("TASK_GATE_NOT_FOUND", "Unknown human gate")
	case errors.Is(err, domain.ErrHumanGateNotPending):
		return apierr.Conflict("TASK_GATE_NOT_PENDING", "This human gate is already resolved", nil)
	case errors.Is(err, domain.ErrHumanGateExists):
		return apierr.Conflict("HUMAN_GATE_PENDING", "This task already has a pending human gate", nil)
	case errors.Is(err, domain.ErrHumanGateKeyReused):
		return apierr.Conflict("IDEMPOTENCY_KEY_REUSED", "requestKey was already used for a different gate", nil)
	default:
		return apierr.Internal("TASK_GATE_FAILED", "Failed to update the human gate")
	}
}
