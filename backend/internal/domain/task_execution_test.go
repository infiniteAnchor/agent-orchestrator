package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The task state machine is durable behaviour, not a display concern: the ready
// queue compares against these states to decide whether a transition happened at
// all, so an accidental extra edge would let a bug masquerade as progress.
func TestCanTransitionTask(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		from TaskState
		to   TaskState
		want bool
	}{
		{"queued to claimed is the claim edge", TaskStateQueued, TaskStateClaimed, true},
		{"queued to cancelled abandons before dispatch", TaskStateQueued, TaskStateCancelled, true},
		{"queued cannot jump straight to running", TaskStateQueued, TaskStateRunning, false},
		{"claimed to running", TaskStateClaimed, TaskStateRunning, true},
		{"running to collecting", TaskStateRunning, TaskStateCollecting, true},
		{"collecting to completed", TaskStateCollecting, TaskStateCompleted, true},
		{"collecting to failed", TaskStateCollecting, TaskStateFailed, true},
		// A retry is a new attempt, so the task returns to the queue rather than
		// resurrecting the failed attempt. Cancelling a failed task is also fine.
		{"failed to queued is the retry edge", TaskStateFailed, TaskStateQueued, true},
		{"failed to cancelled", TaskStateFailed, TaskStateCancelled, true},
		{"failed cannot become completed", TaskStateFailed, TaskStateCompleted, false},
		// Reconciliation either adopts the held dispatch or gives it up, and it
		// may also conclude the work did in fact verify.
		{"blocked to running adopts the dispatch", TaskStateBlocked, TaskStateRunning, true},
		{"blocked to queued re-queues the task", TaskStateBlocked, TaskStateQueued, true},
		{"blocked to completed resolves a late verification", TaskStateBlocked, TaskStateCompleted, true},
		{"blocked to failed gives up", TaskStateBlocked, TaskStateFailed, true},
		{"completed is terminal", TaskStateCompleted, TaskStateRunning, false},
		{"cancelled is terminal", TaskStateCancelled, TaskStateClaimed, false},
		{"a repeated state is not a transition", TaskStateRunning, TaskStateRunning, false},
		{"an unknown state is refused", TaskState("ready"), TaskStateClaimed, false},
		{"an unknown target is refused", TaskStateQueued, TaskState("nope"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := CanTransitionTask(tc.from, tc.to); got != tc.want {
				t.Fatalf("CanTransitionTask(%s, %s) = %v, want %v", tc.from, tc.to, got, tc.want)
			}
		})
	}
}

// There is deliberately no "ready" state. If one is ever added, this test is the
// place that should loudly fail: readiness is derived from verified results at
// read time, and storing it would let a stale row claim readiness the durable
// facts contradict (AGENTS.md: do not store derived status).
func TestTaskStateHasNoStoredReadyState(t *testing.T) {
	t.Parallel()
	for _, state := range []TaskState{
		TaskStateQueued,
		TaskStateClaimed,
		TaskStateRunning,
		TaskStateCollecting,
		TaskStateCompleted,
		TaskStateFailed,
		TaskStateBlocked,
		TaskStateCancelled,
	} {
		if !state.Valid() {
			t.Fatalf("%s should be a valid state", state)
		}
	}
	if TaskState("ready").Valid() {
		t.Fatal("readiness must not be a stored task state")
	}
}

func TestCanTransitionTaskAttempt(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		from TaskAttemptState
		to   TaskAttemptState
		want bool
	}{
		{"a claim starts at claimed", TaskAttemptStateClaimed, TaskAttemptStateRunning, true},
		{"a claim can be abandoned", TaskAttemptStateClaimed, TaskAttemptStateCancelled, true},
		{"running to collecting", TaskAttemptStateRunning, TaskAttemptStateCollecting, true},
		{"collecting to completed", TaskAttemptStateCollecting, TaskAttemptStateCompleted, true},
		// A result may only be recorded during collection, so a jump from running
		// straight to completed must not be reachable.
		{"running cannot skip collecting", TaskAttemptStateRunning, TaskAttemptStateCompleted, false},
		{"blocked to running is reconciliation", TaskAttemptStateBlocked, TaskAttemptStateRunning, true},
		{"blocked to completed resolves a late verification", TaskAttemptStateBlocked, TaskAttemptStateCompleted, true},
		{"completed is terminal", TaskAttemptStateCompleted, TaskAttemptStateBlocked, false},
		{"failed is terminal", TaskAttemptStateFailed, TaskAttemptStateRunning, false},
		{"cancelled is terminal", TaskAttemptStateCancelled, TaskAttemptStateRunning, false},
		{"a repeated state is not a transition", TaskAttemptStateRunning, TaskAttemptStateRunning, false},
		{"an unknown state is refused", TaskAttemptState("queued"), TaskAttemptStateRunning, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := CanTransitionTaskAttempt(tc.from, tc.to); got != tc.want {
				t.Fatalf("CanTransitionTaskAttempt(%s, %s) = %v, want %v", tc.from, tc.to, got, tc.want)
			}
		})
	}
}

func TestTaskAttemptStateTerminal(t *testing.T) {
	t.Parallel()

	terminal := map[TaskAttemptState]bool{
		TaskAttemptStateClaimed:    false,
		TaskAttemptStateRunning:    false,
		TaskAttemptStateCollecting: false,
		TaskAttemptStateCompleted:  true,
		TaskAttemptStateFailed:     true,
		TaskAttemptStateCancelled:  true,
		// Blocked is held, not settled: reconciliation must still be able to move
		// it, so treating it as terminal would strand the work.
		TaskAttemptStateBlocked: false,
	}
	for state, want := range terminal {
		if got := state.Terminal(); got != want {
			t.Fatalf("%s.Terminal() = %v, want %v", state, got, want)
		}
	}
}

// Only a verified result unlocks dependents. An inconclusive outcome holds its
// attempt rather than failing it, because an unproven outcome is not a proven
// failure (AGENTS.md: a failed or unknown runtime probe is not proof of death).
func TestAttemptStateForOutcome(t *testing.T) {
	t.Parallel()

	cases := map[TaskResultOutcome]TaskAttemptState{
		TaskResultVerified:     TaskAttemptStateCompleted,
		TaskResultFailed:       TaskAttemptStateFailed,
		TaskResultInconclusive: TaskAttemptStateBlocked,
	}
	for outcome, want := range cases {
		if got := AttemptStateForOutcome(outcome); got != want {
			t.Fatalf("AttemptStateForOutcome(%s) = %s, want %s", outcome, got, want)
		}
		if got := AttemptStateForOutcome(outcome); got != "" && got.Terminal() == (outcome == TaskResultInconclusive) {
			t.Fatalf("outcome %s maps to %s, which has the wrong terminality", outcome, got)
		}
	}
	if got := AttemptStateForOutcome(TaskResultOutcome("maybe")); got != "" {
		t.Fatalf("an unknown outcome must have no attempt state, got %s", got)
	}

	// Every outcome must be reachable from the state a collector records from,
	// or a result could never be recorded at all.
	for outcome := range cases {
		settled := AttemptStateForOutcome(outcome)
		if !CanTransitionTaskAttempt(TaskAttemptStateCollecting, settled) {
			t.Fatalf("collecting -> %s (%s) must be a legal settle", settled, outcome)
		}
	}
}

func TestTaskAttemptValidate(t *testing.T) {
	t.Parallel()

	started := time.Now()
	finished := started.Add(time.Minute)
	base := TaskAttempt{
		ID:            "attempt-1",
		PlanID:        "plan-1",
		TaskID:        "t1",
		AttemptNumber: 1,
		State:         TaskAttemptStateClaimed,
		ClaimedAt:     started,
		CreatedAt:     started,
		UpdatedAt:     started,
	}

	cases := []struct {
		name string
		edit func(*TaskAttempt)
		want string
	}{
		{"valid claim with no session or harness", func(a *TaskAttempt) {}, ""},
		{"id required", func(a *TaskAttempt) { a.ID = " " }, "task attempt id is required"},
		{"id whitespace", func(a *TaskAttempt) { a.ID = " attempt-1" }, "must not have leading or trailing whitespace"},
		{"plan id required", func(a *TaskAttempt) { a.PlanID = "" }, "task attempt plan ID is required"},
		{"task id required", func(a *TaskAttempt) { a.TaskID = "" }, "task attempt task ID is required"},
		{"attempt number must be positive", func(a *TaskAttempt) { a.AttemptNumber = 0 }, "task attempt number must be positive"},
		{"unknown state", func(a *TaskAttempt) { a.State = "ready" }, "task attempt state \"ready\" is unknown"},
		{"session whitespace", func(a *TaskAttempt) { a.SessionID = "s1 " }, "session ID must not have leading or trailing whitespace"},
		{"claimed at required", func(a *TaskAttempt) { a.ClaimedAt = time.Time{} }, "task attempt claimed-at is required"},
		// A nil StartedAt with a set FinishedAt is a nil-dereference trap, not
		// just a logical error, so it is covered explicitly.
		{"cannot finish before starting", func(a *TaskAttempt) { a.FinishedAt = &finished }, "cannot finish before it started"},
		{"a finished attempt is valid once started", func(a *TaskAttempt) { a.StartedAt = &started; a.FinishedAt = &finished }, ""},
		{"a claimed attempt with no timestamps to finish is valid", func(a *TaskAttempt) { a.StartedAt = &started }, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			attempt := base
			tc.edit(&attempt)
			err := attempt.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestTaskResultValidate(t *testing.T) {
	t.Parallel()

	base := TaskResult{
		ID:         "result-1",
		PlanID:     "plan-1",
		TaskID:     "t1",
		AttemptID:  "attempt-1",
		Outcome:    TaskResultVerified,
		Evidence:   json.RawMessage(`{"commands":[]}`),
		RecordedAt: time.Now(),
	}

	cases := []struct {
		name string
		edit func(*TaskResult)
		want string
	}{
		{"valid verified result", func(r *TaskResult) {}, ""},
		{"id required", func(r *TaskResult) { r.ID = "" }, "task result id is required"},
		{"plan id required", func(r *TaskResult) { r.PlanID = "" }, "task result plan ID is required"},
		{"task id required", func(r *TaskResult) { r.TaskID = "" }, "task result task ID is required"},
		{"attempt id required", func(r *TaskResult) { r.AttemptID = "" }, "task result attempt ID is required"},
		{"unknown outcome", func(r *TaskResult) { r.Outcome = "probably" }, "task result outcome \"probably\" is unknown"},
		{"evidence required", func(r *TaskResult) { r.Evidence = nil }, "task result evidence is required"},
		// Evidence is narrowed by a projection later, and a projection can only
		// narrow structured evidence, never scrape prose.
		{"evidence must be json", func(r *TaskResult) { r.Evidence = json.RawMessage(`not json`) }, "must be valid JSON"},
		{"recorded at required", func(r *TaskResult) { r.RecordedAt = time.Time{} }, "task result recorded-at is required"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result := base
			tc.edit(&result)
			err := result.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}
