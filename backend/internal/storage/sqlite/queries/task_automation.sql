-- name: GetPlannerCursor :one
SELECT consumer, last_seq, updated_at
FROM planner_event_cursor
WHERE consumer = ?;

-- name: InsertPlannerCursor :exec
INSERT INTO planner_event_cursor (consumer, last_seq, updated_at)
VALUES (?, ?, ?);

-- name: AdvancePlannerCursor :execrows
UPDATE planner_event_cursor
SET last_seq = ?, updated_at = ?
WHERE consumer = ? AND last_seq < ?;

-- name: GetPlannerFollowupBySeq :one
SELECT id, project_id, plan_id, task_id, attempt_id, source_seq, state,
       turn_id, orchestrator_id, error_code, summary, created_at, updated_at
FROM planner_followup
WHERE source_seq = ?;

-- name: ListPendingPlannerFollowups :many
SELECT id, project_id, plan_id, task_id, attempt_id, source_seq, state,
       turn_id, orchestrator_id, error_code, summary, created_at, updated_at
FROM planner_followup
WHERE state IN ('queued', 'running')
ORDER BY source_seq, id;

-- name: InsertPlannerFollowup :exec
INSERT INTO planner_followup (
    id, project_id, plan_id, task_id, attempt_id, source_seq, state,
    turn_id, orchestrator_id, error_code, summary, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: SetPlannerFollowupState :execrows
UPDATE planner_followup
SET state = ?, turn_id = ?, orchestrator_id = ?, error_code = ?, summary = ?, updated_at = ?
WHERE id = ? AND state = ?;

-- name: InsertTaskHandoff :exec
INSERT INTO task_handoff (id, project_id, plan_id, task_id, attempt_id, summary, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: ListTaskHandoffs :many
SELECT id, project_id, plan_id, task_id, attempt_id, summary, created_at
FROM task_handoff
WHERE plan_id = ? AND task_id = ?
ORDER BY created_at, id;

-- name: GetTaskRetryPolicy :one
SELECT project_id, max_attempts, fallback_harness, updated_at
FROM task_retry_policy
WHERE project_id = ?;

-- name: UpsertTaskRetryPolicy :exec
INSERT INTO task_retry_policy (project_id, max_attempts, fallback_harness, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT (project_id) DO UPDATE SET
    max_attempts = excluded.max_attempts,
    fallback_harness = excluded.fallback_harness,
    updated_at = excluded.updated_at;

-- name: GetTaskRetryDecision :one
SELECT id, project_id, plan_id, task_id, attempt_id, action, harness, reason, created_at
FROM task_retry_decision
WHERE attempt_id = ?;

-- name: InsertTaskRetryDecision :exec
INSERT INTO task_retry_decision (
    id, project_id, plan_id, task_id, attempt_id, action, harness, reason, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: CountTaskAttempts :one
SELECT COUNT(*) FROM task_attempt WHERE plan_id = ? AND task_id = ?;

-- name: RequeueFailedTask :execrows
UPDATE task
SET state = 'queued', harness = ?, updated_at = ?
WHERE plan_id = ? AND id = ? AND state = 'failed';

-- name: PendingTaskHumanGate :one
SELECT id, project_id, plan_id, task_id, attempt_id, request_key, state, summary,
       created_at, updated_at, resolved_at
FROM task_human_gate
WHERE plan_id = ? AND task_id = ? AND state = 'pending';

-- name: GetTaskHumanGateByRequest :one
SELECT id, project_id, plan_id, task_id, attempt_id, request_key, state, summary,
       created_at, updated_at, resolved_at
FROM task_human_gate
WHERE project_id = ? AND request_key = ?;

-- name: GetTaskHumanGate :one
SELECT id, project_id, plan_id, task_id, attempt_id, request_key, state, summary,
       created_at, updated_at, resolved_at
FROM task_human_gate
WHERE project_id = ? AND id = ?;

-- name: ListTaskHumanGates :many
SELECT id, project_id, plan_id, task_id, attempt_id, request_key, state, summary,
       created_at, updated_at, resolved_at
FROM task_human_gate
WHERE project_id = ? AND plan_id = ?
ORDER BY created_at, id;

-- name: InsertTaskHumanGate :exec
INSERT INTO task_human_gate (
    id, project_id, plan_id, task_id, attempt_id, request_key, state, summary,
    created_at, updated_at, resolved_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ResolveTaskHumanGate :execrows
UPDATE task_human_gate
SET state = ?, updated_at = ?, resolved_at = ?
WHERE id = ? AND project_id = ? AND state = 'pending';
