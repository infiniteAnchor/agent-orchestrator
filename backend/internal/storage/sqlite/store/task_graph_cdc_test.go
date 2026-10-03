package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/cdc"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// Task-graph CDC is captured by DB triggers into change_log (migration 0129),
// never emitted by the store. These tests replay that log, which is the same
// path an SSE client uses to catch up after a reconnect.

// taskEvents returns only the durable task-graph events, in log order.
func taskEvents(t *testing.T, s *sqlite.Store) []cdc.Event {
	t.Helper()
	rows, err := s.EventsAfter(context.Background(), 0, 1000)
	if err != nil {
		t.Fatalf("read change_log: %v", err)
	}
	family := map[cdc.EventType]bool{}
	for _, eventType := range cdc.TaskGraphEventTypes() {
		family[eventType] = true
	}
	out := make([]cdc.Event, 0, len(rows))
	for _, row := range rows {
		if family[row.Type] {
			out = append(out, row)
		}
	}
	return out
}

func payloadKeys(t *testing.T, event cdc.Event) map[string]any {
	t.Helper()
	keys := map[string]any{}
	if err := json.Unmarshal(event.Payload, &keys); err != nil {
		t.Fatalf("decode %s payload %s: %v", event.Type, event.Payload, err)
	}
	return keys
}

// A plan and its tasks announce themselves with camelCase identifiers, and the
// project-level rows carry NO session: a task has no session of its own, and the
// worker session belongs to the attempt it was dispatched on.
func TestTaskGraphCDC_EmitsPlanAndTaskCreationWithoutSession(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "proj-1")
	now := time.Now().UTC().Truncate(time.Second)

	plan := domain.TaskPlan{
		ID:        "plan-1",
		ProjectID: "proj-1",
		Title:     "Ship it",
		Phases:    []domain.TaskPhase{{ID: "p1", Title: "Build"}},
		Tasks: []domain.PlannedTask{
			{ID: "t1", PhaseID: "p1", Title: "One", Prompt: "do one", VerificationCommands: []string{"true"}},
			{ID: "t2", PhaseID: "p1", Title: "Two", Prompt: "do two", DependsOn: []string{"t1"}},
		},
	}
	if _, err := s.CreateTaskPlan(ctx, plan, now); err != nil {
		t.Fatalf("create plan: %v", err)
	}

	events := taskEvents(t, s)
	if len(events) != 3 {
		t.Fatalf("want 3 events (1 plan + 2 tasks, phases are grouping metadata), got %d: %+v", len(events), events)
	}

	planEvent := events[0]
	if planEvent.Type != cdc.EventTaskPlanCreated {
		t.Fatalf("first event = %s, want %s", planEvent.Type, cdc.EventTaskPlanCreated)
	}
	if planEvent.ProjectID != "proj-1" {
		t.Fatalf("plan event project = %q, want proj-1", planEvent.ProjectID)
	}
	if planEvent.SessionID != "" {
		t.Fatalf("a plan event must not carry a session, got %q", planEvent.SessionID)
	}
	planKeys := payloadKeys(t, planEvent)
	if planKeys["id"] != "plan-1" || planKeys["planId"] != "plan-1" || planKeys["title"] != "Ship it" {
		t.Fatalf("plan payload = %v", planKeys)
	}

	for i, wantTask := range []string{"t1", "t2"} {
		event := events[i+1]
		if event.Type != cdc.EventTaskCreated {
			t.Fatalf("event %d = %s, want %s", i+1, event.Type, cdc.EventTaskCreated)
		}
		if event.SessionID != "" {
			t.Fatalf("task %s event must not carry a session, got %q", wantTask, event.SessionID)
		}
		keys := payloadKeys(t, event)
		if keys["id"] != wantTask || keys["taskId"] != wantTask || keys["planId"] != "plan-1" {
			t.Fatalf("task payload = %v", keys)
		}
		if keys["state"] != string(domain.TaskStateQueued) {
			t.Fatalf("a fresh task must announce queued, got %v", keys["state"])
		}
		if _, present := keys["sessionId"]; present {
			t.Fatalf("task payload must omit sessionId entirely, got %v", keys["sessionId"])
		}
	}
}

// Update events fire only for meaningful lifecycle changes: a transition that
// loses its compare-and-swap, or repeats a state, must not emit anything, or
// live clients would be told about work that did not happen.
func TestTaskGraphCDC_TaskUpdateOnlyOnRealStateChange(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "proj-1")
	base := time.Now().UTC().Truncate(time.Second)
	seedTaskGraph(t, s, "proj-1", "plan-1", base)

	before := len(taskEvents(t, s)) // plan + task creation

	if applied, err := s.TransitionTask(ctx, "plan-1", "t1", domain.TaskStateQueued, domain.TaskStateClaimed, base); err != nil || !applied {
		t.Fatalf("claim: applied=%v err=%v", applied, err)
	}
	// Losing CAS: the task is no longer queued, so this must emit nothing.
	if applied, err := s.TransitionTask(ctx, "plan-1", "t1", domain.TaskStateQueued, domain.TaskStateClaimed, base); err != nil || applied {
		t.Fatalf("losing claim: applied=%v err=%v", applied, err)
	}
	events := taskEvents(t, s)
	updates := make([]cdc.Event, 0, 1)
	for _, event := range events[before:] {
		if event.Type == cdc.EventTaskUpdated {
			updates = append(updates, event)
		}
	}
	if len(updates) != 1 {
		t.Fatalf("want exactly 1 task_updated, got %d: %+v", len(updates), updates)
	}
	keys := payloadKeys(t, updates[0])
	if keys["state"] != string(domain.TaskStateClaimed) || keys["taskId"] != "t1" || keys["planId"] != "plan-1" {
		t.Fatalf("task_updated payload = %v", keys)
	}
	if updates[0].SessionID != "" {
		t.Fatalf("task_updated must not carry a session, got %q", updates[0].SessionID)
	}
}

// An attempt is claimed with no session at all, and the session is attached only
// when one is durably associated. The log must show exactly that progression,
// with no synthesized or back-mapped session in between.
func TestTaskGraphCDC_AttemptAttachesSessionOnlyWhenDurable(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "proj-1")
	base := time.Now().UTC().Truncate(time.Second)
	seedTaskGraph(t, s, "proj-1", "plan-1", base)
	session := seedWorkerSession(t, s, "proj-1")

	before := len(taskEvents(t, s))

	if _, err := s.CreateTaskAttempt(ctx, domain.TaskAttempt{
		ID: "attempt-1", PlanID: "plan-1", TaskID: "t1",
	}, base); err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	if _, applied, err := s.TransitionTaskAttempt(
		ctx, "attempt-1", domain.TaskAttemptStateClaimed, domain.TaskAttemptStateRunning, &session, base.Add(time.Second)); err != nil || !applied {
		t.Fatalf("claim -> running: applied=%v err=%v", applied, err)
	}
	// A stale transition from a state the attempt already left emits nothing.
	if _, applied, err := s.TransitionTaskAttempt(
		ctx, "attempt-1", domain.TaskAttemptStateClaimed, domain.TaskAttemptStateRunning, nil, base); err != nil || applied {
		t.Fatalf("stale transition: applied=%v err=%v", applied, err)
	}

	events := taskEvents(t, s)[before:]
	if len(events) != 2 {
		t.Fatalf("want 2 attempt events (claim + running), got %d: %+v", len(events), events)
	}

	created := events[0]
	if created.Type != cdc.EventTaskAttemptCreated {
		t.Fatalf("first event = %s, want %s", created.Type, cdc.EventTaskAttemptCreated)
	}
	if created.SessionID != "" {
		t.Fatalf("an unassociated claim must not carry a session, got %q", created.SessionID)
	}
	createdKeys := payloadKeys(t, created)
	if createdKeys["attemptId"] != "attempt-1" || createdKeys["taskId"] != "t1" || createdKeys["planId"] != "plan-1" {
		t.Fatalf("attempt_created payload = %v", createdKeys)
	}
	if createdKeys["state"] != string(domain.TaskAttemptStateClaimed) {
		t.Fatalf("attempt_created state = %v", createdKeys["state"])
	}

	updated := events[1]
	if updated.Type != cdc.EventTaskAttemptUpdated {
		t.Fatalf("second event = %s, want %s", updated.Type, cdc.EventTaskAttemptUpdated)
	}
	if updated.SessionID != string(session) {
		t.Fatalf("attempt_updated session = %q, want %q", updated.SessionID, session)
	}
	updatedKeys := payloadKeys(t, updated)
	if updatedKeys["sessionId"] != string(session) || updatedKeys["state"] != string(domain.TaskAttemptStateRunning) {
		t.Fatalf("attempt_updated payload = %v", updatedKeys)
	}
}

// The recorded result is the evidence a dependent task unlocks on, so the event
// must carry the outcome and the session the attempt was dispatched on — even
// though the settle path itself passes no session.
func TestTaskGraphCDC_ResultRecordedCarriesOutcomeAndAttemptSession(t *testing.T) {
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
	for _, to := range []domain.TaskAttemptState{domain.TaskAttemptStateRunning, domain.TaskAttemptStateCollecting} {
		from := domain.TaskAttemptStateClaimed
		if to == domain.TaskAttemptStateCollecting {
			from = domain.TaskAttemptStateRunning
		}
		if _, applied, err := s.TransitionTaskAttempt(ctx, "attempt-1", from, to, &session, base); err != nil || !applied {
			t.Fatalf("%s -> %s: applied=%v err=%v", from, to, applied, err)
		}
	}

	before := len(taskEvents(t, s))

	if _, applied, err := s.RecordTaskResult(ctx, domain.TaskResult{
		ID:         "result-1",
		PlanID:     "plan-1",
		TaskID:     "t1",
		AttemptID:  "attempt-1",
		Outcome:    domain.TaskResultVerified,
		Summary:    "passed",
		Evidence:   []byte(`{"commands":[{"command":"true","exitCode":0}]}`),
		RecordedAt: base.Add(time.Minute),
	}, domain.TaskAttemptStateCollecting); err != nil || !applied {
		t.Fatalf("record result: applied=%v err=%v", applied, err)
	}

	events := taskEvents(t, s)[before:]
	resultEvents := make([]cdc.Event, 0, 1)
	for _, event := range events {
		if event.Type == cdc.EventTaskResultRecorded {
			resultEvents = append(resultEvents, event)
		}
	}
	if len(resultEvents) != 1 {
		t.Fatalf("want exactly 1 task_result_recorded, got %d: %+v", len(resultEvents), events)
	}
	event := resultEvents[0]
	if event.SessionID != string(session) {
		t.Fatalf("result event session = %q, want the attempt's %q", event.SessionID, session)
	}
	keys := payloadKeys(t, event)
	if keys["outcome"] != string(domain.TaskResultVerified) ||
		keys["attemptId"] != "attempt-1" ||
		keys["taskId"] != "t1" ||
		keys["planId"] != "plan-1" ||
		keys["sessionId"] != string(session) {
		t.Fatalf("task_result_recorded payload = %v", keys)
	}
	// Raw command output is deliberately not in the event: slice 3 projects
	// evidence for remote clients, so it must not be broadcast verbatim.
	if _, leaked := keys["evidence"]; leaked {
		t.Fatalf("result event must not carry raw evidence, got %v", keys)
	}
}

// The result event must not appear before the attempt transition it justifies:
// evidence and the state it settles are one transaction.
func TestTaskGraphCDC_ResultSettlesAttemptInSameTransaction(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "proj-1")
	base := time.Now().UTC().Truncate(time.Second)
	seedTaskGraph(t, s, "proj-1", "plan-1", base)
	session := seedWorkerSession(t, s, "proj-1")

	if _, err := s.CreateTaskAttempt(ctx, domain.TaskAttempt{
		ID: "attempt-1", PlanID: "plan-1", TaskID: "t1",
	}, base); err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	if _, applied, err := s.TransitionTaskAttempt(
		ctx, "attempt-1", domain.TaskAttemptStateClaimed, domain.TaskAttemptStateRunning, &session, base); err != nil || !applied {
		t.Fatalf("claim -> running: applied=%v err=%v", applied, err)
	}
	if _, applied, err := s.TransitionTaskAttempt(
		ctx, "attempt-1", domain.TaskAttemptStateRunning, domain.TaskAttemptStateCollecting, &session, base); err != nil || !applied {
		t.Fatalf("running -> collecting: applied=%v err=%v", applied, err)
	}

	// An inconclusive outcome holds the attempt at blocked instead of failing it,
	// and still records its evidence.
	if _, applied, err := s.RecordTaskResult(ctx, domain.TaskResult{
		ID: "result-1", PlanID: "plan-1", TaskID: "t1", AttemptID: "attempt-1",
		Outcome: domain.TaskResultInconclusive, Evidence: []byte(`{"reason":"probe failed"}`),
		RecordedAt: base.Add(time.Minute),
	}, domain.TaskAttemptStateCollecting); err != nil || !applied {
		t.Fatalf("record inconclusive result: applied=%v err=%v", applied, err)
	}

	attempt, ok, err := s.GetTaskAttempt(ctx, "attempt-1")
	if err != nil || !ok {
		t.Fatalf("get attempt: ok=%v err=%v", ok, err)
	}
	if attempt.State != domain.TaskAttemptStateBlocked {
		t.Fatalf("an inconclusive result must hold the attempt, got %s", attempt.State)
	}
	// The session association survives the settle.
	if attempt.SessionID != string(session) {
		t.Fatalf("settling cleared the session association: %q", attempt.SessionID)
	}

	verified, err := s.ListVerifiedTaskResults(ctx, "plan-1")
	if err != nil {
		t.Fatalf("list verified: %v", err)
	}
	if len(verified) != 0 {
		t.Fatalf("an inconclusive result must not unlock dependents, got %+v", verified)
	}
}
