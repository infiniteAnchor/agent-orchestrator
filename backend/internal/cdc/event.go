// Package cdc is the change-data-capture delivery layer. Change events are
// captured durably by SQLite triggers into the change_log table (see the storage
// migrations); this package POLLS that log and fans new events out, in order, to
// in-process subscribers such as terminal session-state fan-out. Future SSE/event
// endpoints can subscribe here too.
//
// There is no durable outbox/JSONL/janitor machinery: the change_log table IS
// the durable, ordered source of truth, and clients catch up by reading it from
// their own offset (SSE Last-Event-ID). The poller + broadcaster here are only
// the LIVE push on top of that.
package cdc

import (
	"encoding/json"
	"time"
)

// EventType mirrors the event_type values the DB triggers write.
type EventType string

// Event types, one per row-change the DB triggers emit into change_log.
const (
	EventSessionCreated         EventType = "session_created"
	EventSessionUpdated         EventType = "session_updated"
	EventPRCreated              EventType = "pr_created"
	EventPRUpdated              EventType = "pr_updated"
	EventPRCheckRecorded        EventType = "pr_check_recorded"
	EventPRSessionChanged       EventType = "pr_session_changed"
	EventPRReviewThreadAdded    EventType = "pr_review_thread_added"
	EventPRReviewThreadResolved EventType = "pr_review_thread_resolved"
	EventReviewRunCreated       EventType = "review_run_created"
	EventReviewRunUpdated       EventType = "review_run_updated"

	// Durable task-graph lifecycle. This package owns the vocabulary; the DB
	// triggers in migration 0129 write exactly these strings and the
	// change_log CHECK constraint admits only these.
	EventTaskPlanCreated    EventType = "task_plan_created"
	EventTaskCreated        EventType = "task_created"
	EventTaskUpdated        EventType = "task_updated"
	EventTaskAttemptCreated EventType = "task_attempt_created"
	EventTaskAttemptUpdated EventType = "task_attempt_updated"
	EventTaskResultRecorded EventType = "task_result_recorded"
)

// Task-graph payload contract. Every task-graph event carries the same stable
// identifiers in camelCase, so a consumer can key off any of them without
// knowing which trigger fired:
//
//	id, planId, taskId, attemptId, sessionId, state, outcome
//
// Not every key is present on every event - a plan event has no taskId, a task
// event has no attemptId - and absent keys are omitted rather than sent as null.
//
// sessionId is populated ONLY from a durable association. Task-level events
// (task_plan_created, task_created, task_updated) carry a NULL session_id
// because the task itself has no session; the worker session is attached to the
// ATTEMPT it was dispatched on, and attempt events carry it. A task-level event
// never guesses or back-maps a session, and pre-association attempts
// (task_attempt_created before a session is associated) legitimately carry a
// null sessionId.
//
// Update events are emitted only for meaningful lifecycle changes: the task and
// attempt update triggers are guarded on state (and, for attempts, on a newly
// durable session association), so re-reading or re-probing an attempt emits
// nothing.

// TaskGraphEventPayload is the decoded shape of the task-graph payloads above.
// Keys are omitted when the trigger did not supply them; SessionID is empty for
// task-level and pre-association events.
type TaskGraphEventPayload struct {
	ID        string `json:"id"`
	PlanID    string `json:"planId,omitempty"`
	TaskID    string `json:"taskId,omitempty"`
	AttemptID string `json:"attemptId,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	State     string `json:"state,omitempty"`
	Outcome   string `json:"outcome,omitempty"`
	Title     string `json:"title,omitempty"`
}

// TaskGraphEventTypes lists the durable task-graph event types, so callers that
// need to recognize the family (rather than one member) do not hand-roll a
// switch that drifts when a type is added here.
func TaskGraphEventTypes() []EventType {
	return []EventType{
		EventTaskPlanCreated,
		EventTaskCreated,
		EventTaskUpdated,
		EventTaskAttemptCreated,
		EventTaskAttemptUpdated,
		EventTaskResultRecorded,
	}
}

// Event is one CDC change read from change_log. Seq is the monotonic ordering +
// idempotency key (consumers dedup by it). SessionID is empty for project-level
// events. Payload is the trigger-built JSON, kept raw so a typed transport can
// narrow it by Type (the discriminated-union decode lives at the transport edge,
// not here).
type Event struct {
	Seq       int64           `json:"seq"`
	ProjectID string          `json:"projectId"`
	SessionID string          `json:"sessionId,omitempty"`
	Type      EventType       `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"createdAt"`
}
