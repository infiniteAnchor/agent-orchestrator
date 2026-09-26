package domain

import (
	"errors"
	"time"
	"unicode/utf8"
)

var (
	ErrPlannerCursorMissing  = errors.New("domain: planner cursor is not initialized")
	ErrTaskRetryAmbiguous    = errors.New("domain: ambiguous task attempt cannot be retried")
	ErrTaskRetryNotFailed    = errors.New("domain: task attempt is not a retryable failure")
	ErrHumanGatePending      = errors.New("domain: a human gate is pending")
	ErrHumanGateNotPending   = errors.New("domain: human gate is not pending")
	ErrHumanGateExists       = errors.New("domain: task already has a pending human gate")
	ErrHumanGateKeyReused    = errors.New("domain: human gate request key was reused")
	ErrTaskAutomationMissing = errors.New("domain: task automation record was not found")
)

// Follow-up and handoff bounds. One task-result event creates at most one
// planner turn. Summary text is capped in UTF-8 bytes.
const (
	MaxFollowupsPerEvent   = 1
	MaxHandoffSummaryBytes = 4096
	DefaultMaxTaskAttempts = 2
	MaxTaskAttemptsCap     = 8

	// RetryReasonFailed is an ordinary verified failure.
	RetryReasonFailed = "failed"
	// RetryReasonProviderExhausted asks for the configured fallback harness.
	RetryReasonProviderExhausted = "provider_exhausted"

	// PlannerConsumerTaskResults is the durable cursor that tails task results.
	PlannerConsumerTaskResults = "task-results"
)

// ClaimHumanGate refuses a claim while a person still has to resolve a gate.
const ClaimHumanGate = "human_gate"

// RetryAction is the durable choice for one settled attempt.
type RetryAction string

const (
	// RetryActionNone means the attempt is not a failure to retry.
	RetryActionNone RetryAction = "none"
	// RetryActionRetry requeues the same task and harness as a new attempt.
	RetryActionRetry RetryAction = "retry"
	// RetryActionFallback requeues the same task on the configured harness.
	RetryActionFallback RetryAction = "fallback"
	// RetryActionEscalate stops automatic dispatch and opens a human gate.
	RetryActionEscalate RetryAction = "escalate"
	// RetryActionHold leaves an ambiguous attempt untouched.
	RetryActionHold RetryAction = "hold"
)

// Valid reports whether the action is one this build persists.
func (a RetryAction) Valid() bool {
	switch a {
	case RetryActionNone, RetryActionRetry, RetryActionFallback, RetryActionEscalate, RetryActionHold:
		return true
	default:
		return false
	}
}

// Requeues reports whether the action moves a failed task back to queued.
func (a RetryAction) Requeues() bool {
	return a == RetryActionRetry || a == RetryActionFallback
}

// RetryInput is the durable facts DecideRetry is allowed to see.
type RetryInput struct {
	Outcome         TaskResultOutcome
	AttemptState    TaskAttemptState
	RuntimeRef      string
	AttemptCount    int
	MaxAttempts     int
	CurrentHarness  string
	FallbackHarness string
	Reason          string
	HasResult       bool
}

// RetryChoice is the pure policy result. Harness is the harness to store when
// Requeues is true. Workspace identity is never part of this choice.
type RetryChoice struct {
	Action  RetryAction
	Harness string
}

// DecideRetry chooses a retry, a fallback, an escalation, or a hold.
// A blocked attempt, an inconclusive result, or a dispatch lease that was
// never resolved to a failed attempt is a hold: an ambiguous launch is not
// retried as a failure. A failed attempt may still carry the dispatch lease
// because a confirmed launch rejection does not clear it; that failed state
// is proof the launcher rejected the start, so it can be retried.
func DecideRetry(in RetryInput) RetryChoice {
	maxAttempts := in.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = DefaultMaxTaskAttempts
	}
	if maxAttempts > MaxTaskAttemptsCap {
		maxAttempts = MaxTaskAttemptsCap
	}
	ambiguous := in.AttemptState == TaskAttemptStateBlocked || in.Outcome == TaskResultInconclusive
	if !ambiguous && in.RuntimeRef == TaskAttemptDispatchLease && in.AttemptState != TaskAttemptStateFailed {
		ambiguous = true
	}
	if ambiguous {
		return RetryChoice{Action: RetryActionHold}
	}
	failed := in.AttemptState == TaskAttemptStateFailed && (in.Outcome == TaskResultFailed || !in.HasResult)
	if !failed {
		return RetryChoice{Action: RetryActionNone}
	}
	if in.Reason == RetryReasonProviderExhausted && in.FallbackHarness != "" && in.FallbackHarness != in.CurrentHarness {
		return RetryChoice{Action: RetryActionFallback, Harness: in.FallbackHarness}
	}
	if in.AttemptCount < maxAttempts {
		return RetryChoice{Action: RetryActionRetry, Harness: in.CurrentHarness}
	}
	return RetryChoice{Action: RetryActionEscalate}
}

// PlannerFollowupState is the lifecycle of one event-driven reviewer turn.
type PlannerFollowupState string

const (
	PlannerFollowupQueued    PlannerFollowupState = "queued"
	PlannerFollowupRunning   PlannerFollowupState = "running"
	PlannerFollowupCompleted PlannerFollowupState = "completed"
	PlannerFollowupFailed    PlannerFollowupState = "failed"
	PlannerFollowupSkipped   PlannerFollowupState = "skipped"
)

// Valid reports whether state is persisted.
func (s PlannerFollowupState) Valid() bool {
	switch s {
	case PlannerFollowupQueued, PlannerFollowupRunning, PlannerFollowupCompleted, PlannerFollowupFailed, PlannerFollowupSkipped:
		return true
	default:
		return false
	}
}

// PlannerFollowup is the one reviewer turn claimed for one change_log event.
type PlannerFollowup struct {
	ID             string
	ProjectID      ProjectID
	PlanID         string
	TaskID         string
	AttemptID      string
	SourceSeq      int64
	State          PlannerFollowupState
	TurnID         string
	OrchestratorID SessionID
	ErrorCode      string
	Summary        string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// TaskHandoff is a bounded summary linked to one task attempt.
type TaskHandoff struct {
	ID        string
	ProjectID ProjectID
	PlanID    string
	TaskID    string
	AttemptID string
	Summary   string
	CreatedAt time.Time
}

// HumanGateState is the resolution of work that is waiting on a person.
type HumanGateState string

const (
	HumanGatePending  HumanGateState = "pending"
	HumanGateApproved HumanGateState = "approved"
	HumanGateRejected HumanGateState = "rejected"
)

// Valid reports whether state is persisted.
func (s HumanGateState) Valid() bool {
	switch s {
	case HumanGatePending, HumanGateApproved, HumanGateRejected:
		return true
	default:
		return false
	}
}

// HumanGate is a restart-safe approval. The row is the durable notification.
type HumanGate struct {
	ID         string
	ProjectID  ProjectID
	PlanID     string
	TaskID     string
	AttemptID  string
	RequestKey string
	State      HumanGateState
	Summary    string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	ResolvedAt *time.Time
}

// TaskRetryPolicy is the per-project retry and fallback configuration.
type TaskRetryPolicy struct {
	ProjectID       ProjectID
	MaxAttempts     int
	FallbackHarness string
	UpdatedAt       time.Time
}

// Normalized clamps the attempt cap and leaves an empty fallback empty.
func (p TaskRetryPolicy) Normalized() TaskRetryPolicy {
	if p.MaxAttempts < 1 {
		p.MaxAttempts = DefaultMaxTaskAttempts
	}
	if p.MaxAttempts > MaxTaskAttemptsCap {
		p.MaxAttempts = MaxTaskAttemptsCap
	}
	return p
}

// TaskRetryDecision is the idempotent record of one attempt's policy choice.
type TaskRetryDecision struct {
	ID        string
	ProjectID ProjectID
	PlanID    string
	TaskID    string
	AttemptID string
	Action    RetryAction
	Harness   string
	Reason    string
	CreatedAt time.Time
}

// AppliedResultEvent is what one consumed task-result event persisted.
type AppliedResultEvent struct {
	Followup PlannerFollowup
	Decision *TaskRetryDecision
	Gate     *HumanGate
	Created  bool
}

// RetryOutcome is one explicit or automatic retry decision.
type RetryOutcome struct {
	Decision TaskRetryDecision
	Gate     *HumanGate
	Repeated bool
}

// ValidateHandoffSummary rejects an empty or oversized user-authored summary.
// The limit is UTF-8 bytes.
func ValidateHandoffSummary(summary string) error {
	if summary == "" {
		return errors.New("handoff summary is required")
	}
	if len([]byte(summary)) > MaxHandoffSummaryBytes {
		return errors.New("handoff summary exceeds 4096 bytes")
	}
	return nil
}

// TruncateHandoffSummary caps model output on a UTF-8 boundary.
func TruncateHandoffSummary(summary string) string {
	b := []byte(summary)
	if len(b) <= MaxHandoffSummaryBytes {
		return summary
	}
	b = b[:MaxHandoffSummaryBytes]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}
