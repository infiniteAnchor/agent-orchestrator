package domain

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// TaskState is the durable lifecycle state of one task in a plan.
//
// There is deliberately no "ready" state: readiness is DERIVED at read time from
// verified task results plus the dependency edges, never stored. Storing it
// would let a stale row claim readiness the durable facts contradict. See
// docs/plans/phase-3-durable-task-graph.md ("Ready queue").
type TaskState string

// Task lifecycle states. This mirrors the state list in
// docs/headless-server-control-plane.md; keep the two in sync.
const (
	TaskStateQueued     TaskState = "queued"
	TaskStateClaimed    TaskState = "claimed"
	TaskStateRunning    TaskState = "running"
	TaskStateCollecting TaskState = "collecting"
	TaskStateCompleted  TaskState = "completed"
	TaskStateFailed     TaskState = "failed"
	TaskStateBlocked    TaskState = "blocked"
	TaskStateCancelled  TaskState = "cancelled"
)

// Valid reports whether state is one this build knows how to persist.
func (s TaskState) Valid() bool {
	switch s {
	case TaskStateQueued, TaskStateClaimed, TaskStateRunning, TaskStateCollecting,
		TaskStateCompleted, TaskStateFailed, TaskStateBlocked, TaskStateCancelled:
		return true
	default:
		return false
	}
}

// taskTransitions is the legal task-state machine. failed -> queued is the retry
// edge (a retry is a new attempt, not a resurrected one). blocked -> running or
// blocked -> queued is the reconciliation edge: recovery either adopts the
// ambiguous dispatch or re-queues it. completed and cancelled are terminal.
var taskTransitions = map[TaskState][]TaskState{
	TaskStateQueued:     {TaskStateClaimed, TaskStateCancelled},
	TaskStateClaimed:    {TaskStateRunning, TaskStateBlocked, TaskStateFailed, TaskStateCancelled},
	TaskStateRunning:    {TaskStateCollecting, TaskStateBlocked, TaskStateFailed, TaskStateCancelled},
	TaskStateCollecting: {TaskStateCompleted, TaskStateBlocked, TaskStateFailed, TaskStateCancelled},
	TaskStateFailed:     {TaskStateQueued, TaskStateCancelled},
	TaskStateBlocked:    {TaskStateQueued, TaskStateRunning, TaskStateCollecting, TaskStateCompleted, TaskStateFailed, TaskStateCancelled},
	TaskStateCompleted:  {},
	TaskStateCancelled:  {},
}

// CanTransitionTask reports whether from -> to is a legal task transition.
// A transition to the same state is not legal: it would emit a no-op CDC event
// and hide a double-dispatch bug.
func CanTransitionTask(from, to TaskState) bool {
	if !from.Valid() || !to.Valid() || from == to {
		return false
	}
	for _, allowed := range taskTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// TaskAttemptState is the durable lifecycle state of one dispatch attempt. An
// attempt is the unit of idempotency: it is persisted before the runtime is
// launched so a crash between claim and launch is reconcilable rather than
// duplicated.
type TaskAttemptState string

// Attempt lifecycle states. An attempt begins at claimed (the claim IS the
// insert) and so has no queued state of its own.
const (
	TaskAttemptStateClaimed    TaskAttemptState = "claimed"
	TaskAttemptStateRunning    TaskAttemptState = "running"
	TaskAttemptStateCollecting TaskAttemptState = "collecting"
	TaskAttemptStateCompleted  TaskAttemptState = "completed"
	TaskAttemptStateFailed     TaskAttemptState = "failed"
	TaskAttemptStateBlocked    TaskAttemptState = "blocked"
	TaskAttemptStateCancelled  TaskAttemptState = "cancelled"
)

// Valid reports whether state is one this build knows how to persist.
func (s TaskAttemptState) Valid() bool {
	switch s {
	case TaskAttemptStateClaimed, TaskAttemptStateRunning, TaskAttemptStateCollecting,
		TaskAttemptStateCompleted, TaskAttemptStateFailed, TaskAttemptStateBlocked,
		TaskAttemptStateCancelled:
		return true
	default:
		return false
	}
}

// Terminal reports whether the attempt has settled and can no longer be
// transitioned. A blocked attempt is NOT terminal: it is held for
// reconciliation, which resolves it to a running or failed attempt.
func (s TaskAttemptState) Terminal() bool {
	switch s {
	case TaskAttemptStateCompleted, TaskAttemptStateFailed, TaskAttemptStateCancelled:
		return true
	default:
		return false
	}
}

var attemptTransitions = map[TaskAttemptState][]TaskAttemptState{
	TaskAttemptStateClaimed:    {TaskAttemptStateRunning, TaskAttemptStateBlocked, TaskAttemptStateFailed, TaskAttemptStateCancelled},
	TaskAttemptStateRunning:    {TaskAttemptStateCollecting, TaskAttemptStateBlocked, TaskAttemptStateFailed, TaskAttemptStateCancelled},
	TaskAttemptStateCollecting: {TaskAttemptStateCompleted, TaskAttemptStateBlocked, TaskAttemptStateFailed, TaskAttemptStateCancelled},
	TaskAttemptStateFailed:     {},
	TaskAttemptStateBlocked:    {TaskAttemptStateRunning, TaskAttemptStateCollecting, TaskAttemptStateCompleted, TaskAttemptStateFailed, TaskAttemptStateCancelled},
	TaskAttemptStateCompleted:  {},
	TaskAttemptStateCancelled:  {},
}

// CanTransitionTaskAttempt reports whether from -> to is a legal attempt
// transition. Same-state transitions are rejected for the same reason as
// CanTransitionTask.
func CanTransitionTaskAttempt(from, to TaskAttemptState) bool {
	if !from.Valid() || !to.Valid() || from == to {
		return false
	}
	for _, allowed := range attemptTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// TaskResultOutcome is how a recorded result settles its attempt. Only
// TaskResultVerified unlocks dependents; inconclusive means "held for
// reconciliation" and must never be read as either success or failure.
type TaskResultOutcome string

// Result outcomes.
const (
	TaskResultVerified     TaskResultOutcome = "verified"
	TaskResultFailed       TaskResultOutcome = "failed"
	TaskResultInconclusive TaskResultOutcome = "inconclusive"
)

// Valid reports whether outcome is one this build knows how to persist.
func (o TaskResultOutcome) Valid() bool {
	switch o {
	case TaskResultVerified, TaskResultFailed, TaskResultInconclusive:
		return true
	default:
		return false
	}
}

// AttemptStateForOutcome is the terminal (or held) attempt state a recorded
// result settles its attempt into. Inconclusive holds the attempt at blocked
// rather than failing it, because an unproven outcome is not a proven failure —
// see AGENTS.md "do not treat failed/unknown runtime probes as proof a session
// is dead".
func AttemptStateForOutcome(o TaskResultOutcome) TaskAttemptState {
	switch o {
	case TaskResultVerified:
		return TaskAttemptStateCompleted
	case TaskResultFailed:
		return TaskAttemptStateFailed
	case TaskResultInconclusive:
		return TaskAttemptStateBlocked
	default:
		return ""
	}
}

// TaskPlanSummary is the durable metadata of a stored plan, without its graph.
// Listing plans returns summaries and reading one returns the graph, so a list
// response never has to materialise every task, edge, and command in a project.
// Timestamps live here rather than on TaskPlan because the graph contract in
// task_graph.go is the proposed/validated shape, and validation must not depend
// on when a row happened to be written.
type TaskPlanSummary struct {
	ID        string
	ProjectID string
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TaskAttempt is one durable dispatch of one task. SessionID and Harness are
// empty until durably known; a claim is recorded before either is resolved, so
// an attempt row can legitimately exist with neither. SessionID is never
// back-mapped from the task — it is set only when a worker session is actually
// associated with this attempt.
type TaskAttempt struct {
	ID            string
	PlanID        string
	TaskID        string
	AttemptNumber int
	State         TaskAttemptState
	SessionID     string
	Harness       AgentHarness
	// RuntimeRef is the launcher's opaque worker identity. Empty until dispatch
	// begins. TaskAttemptDispatchLease means a launch was attempted and the
	// real identity is not durable yet. It must never be a host path.
	RuntimeRef string
	ClaimedAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Validate checks an attempt record without consulting persistence.
func (a TaskAttempt) Validate() error {
	if strings.TrimSpace(a.ID) == "" {
		return fmt.Errorf("task attempt id is required")
	}
	if a.ID != strings.TrimSpace(a.ID) {
		return fmt.Errorf("task attempt id must not have leading or trailing whitespace")
	}
	if strings.TrimSpace(a.PlanID) == "" {
		return fmt.Errorf("task attempt plan ID is required")
	}
	if strings.TrimSpace(a.TaskID) == "" {
		return fmt.Errorf("task attempt task ID is required")
	}
	if a.AttemptNumber < 1 {
		return fmt.Errorf("task attempt number must be positive")
	}
	if !a.State.Valid() {
		return fmt.Errorf("task attempt state %q is unknown", a.State)
	}
	if a.SessionID != strings.TrimSpace(a.SessionID) {
		return fmt.Errorf("task attempt session ID must not have leading or trailing whitespace")
	}
	if err := ValidateRuntimeRef(a.RuntimeRef); err != nil {
		return err
	}
	if a.ClaimedAt.IsZero() {
		return fmt.Errorf("task attempt claimed-at is required")
	}
	if a.FinishedAt != nil && a.StartedAt == nil {
		return fmt.Errorf("task attempt cannot finish before it started")
	}
	return nil
}

// ValidateRuntimeRef accepts an empty ref, the dispatch lease, or an opaque
// identifier. Slashes and parent-directory segments are rejected so a launcher
// cannot persist a host path as a worker identity.
func ValidateRuntimeRef(ref string) error {
	if ref == "" || ref == TaskAttemptDispatchLease {
		return nil
	}
	if ref != strings.TrimSpace(ref) {
		return fmt.Errorf("task attempt runtime ref must not have leading or trailing whitespace")
	}
	if strings.ContainsAny(ref, "/\\") || strings.Contains(ref, "..") || len(ref) > 128 {
		return fmt.Errorf("task attempt runtime ref must be an opaque identifier")
	}
	return nil
}

// TaskResult is one immutable verification result for one attempt. It is
// append-only: it is evidence, so it is never updated or deleted. One attempt
// has at most one result, which keeps "verified" unambiguous when deriving
// readiness.
type TaskResult struct {
	ID         string
	PlanID     string
	TaskID     string
	AttemptID  string
	Outcome    TaskResultOutcome
	Summary    string
	Evidence   json.RawMessage
	RecordedAt time.Time
}

// Validate checks a result record without consulting persistence. Evidence must
// be a JSON value: slice 3 projects it for remote clients, and a projection can
// only narrow structured evidence, not scrape prose.
func (r TaskResult) Validate() error {
	if strings.TrimSpace(r.ID) == "" {
		return fmt.Errorf("task result id is required")
	}
	if r.ID != strings.TrimSpace(r.ID) {
		return fmt.Errorf("task result id must not have leading or trailing whitespace")
	}
	if strings.TrimSpace(r.PlanID) == "" {
		return fmt.Errorf("task result plan ID is required")
	}
	if strings.TrimSpace(r.TaskID) == "" {
		return fmt.Errorf("task result task ID is required")
	}
	if strings.TrimSpace(r.AttemptID) == "" {
		return fmt.Errorf("task result attempt ID is required")
	}
	if !r.Outcome.Valid() {
		return fmt.Errorf("task result outcome %q is unknown", r.Outcome)
	}
	if len(r.Evidence) == 0 {
		return fmt.Errorf("task result evidence is required")
	}
	if !json.Valid(r.Evidence) {
		return fmt.Errorf("task result evidence must be valid JSON")
	}
	if r.RecordedAt.IsZero() {
		return fmt.Errorf("task result recorded-at is required")
	}
	return nil
}
