-- Task-graph persistence (slice 2 of the durable task graph). Writes are
-- composed by Store methods inside one transaction: a plan is validated by
-- domain.TaskPlan.Validate before any row is written, so an invalid graph never
-- reaches SQLite. Attempt transitions are compare-and-swap on the current state,
-- which is what makes a concurrent claim lose instead of double-dispatch.
--
-- Nothing here writes change_log. Task events are captured by the triggers in
-- migration 0129, and task_result is append-only at the schema level.

-- name: InsertTaskPlan :exec
INSERT INTO task_plan (id, project_id, title, created_at, updated_at)
VALUES (?, ?, ?, ?, ?);

-- name: InsertTaskPhase :exec
INSERT INTO task_phase (plan_id, id, title, position)
VALUES (?, ?, ?, ?);

-- name: InsertTask :exec
INSERT INTO task (plan_id, id, phase_id, title, prompt, position, state, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: InsertTaskDependency :exec
INSERT INTO task_dependency (plan_id, task_id, depends_on_task_id, position)
VALUES (?, ?, ?, ?);

-- name: InsertTaskVerificationCommand :exec
INSERT INTO task_verification_command (plan_id, task_id, position, command)
VALUES (?, ?, ?, ?);

-- name: GetTaskPlan :one
SELECT id, project_id, title, created_at, updated_at
FROM task_plan
WHERE project_id = ? AND id = ?;

-- name: ListTaskPlans :many
SELECT id, project_id, title, created_at, updated_at
FROM task_plan
WHERE project_id = ?
ORDER BY created_at DESC, id;

-- name: ListTaskPhases :many
SELECT plan_id, id, title, position
FROM task_phase
WHERE plan_id = ?
ORDER BY position;

-- name: ListTasks :many
SELECT plan_id, id, phase_id, title, prompt, position, state, created_at, updated_at
FROM task
WHERE plan_id = ?
ORDER BY position;

-- name: ListTaskDependencies :many
SELECT plan_id, task_id, depends_on_task_id, position
FROM task_dependency
WHERE plan_id = ?
ORDER BY task_id, position;

-- name: ListTaskVerificationCommands :many
SELECT plan_id, task_id, position, command
FROM task_verification_command
WHERE plan_id = ?
ORDER BY task_id, position;

-- name: GetTask :one
SELECT plan_id, id, phase_id, title, prompt, position, state, created_at, updated_at
FROM task
WHERE plan_id = ? AND id = ?;

-- name: TransitionTaskState :execrows
UPDATE task
SET state = ?, updated_at = ?
WHERE plan_id = ? AND id = ? AND state = ?;

-- name: InsertTaskAttempt :exec
INSERT INTO task_attempt (
    id, plan_id, task_id, attempt_number, state, session_id, harness,
    claimed_at, started_at, finished_at, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetTaskAttempt :one
SELECT id, plan_id, task_id, attempt_number, state, session_id, harness,
       claimed_at, started_at, finished_at, created_at, updated_at
FROM task_attempt
WHERE id = ?;

-- name: ListTaskAttempts :many
SELECT id, plan_id, task_id, attempt_number, state, session_id, harness,
       claimed_at, started_at, finished_at, created_at, updated_at
FROM task_attempt
WHERE plan_id = ? AND task_id = ?
ORDER BY attempt_number;

-- name: NextTaskAttemptNumber :one
SELECT CAST(COALESCE(MAX(attempt_number), 0) + 1 AS INTEGER) AS attempt_number
FROM task_attempt
WHERE plan_id = ? AND task_id = ?;

-- name: TransitionTaskAttempt :execrows
UPDATE task_attempt
SET state = ?, session_id = ?, started_at = ?, finished_at = ?, updated_at = ?
WHERE id = ? AND state = ?;

-- name: InsertTaskResult :exec
INSERT INTO task_result (id, plan_id, task_id, attempt_id, outcome, summary, evidence, recorded_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetTaskResultByAttempt :one
SELECT id, plan_id, task_id, attempt_id, outcome, summary, evidence, recorded_at
FROM task_result
WHERE attempt_id = ?;

-- name: ListTaskResults :many
SELECT id, plan_id, task_id, attempt_id, outcome, summary, evidence, recorded_at
FROM task_result
WHERE plan_id = ?
ORDER BY recorded_at, id;

-- name: ListVerifiedTaskResults :many
SELECT id, plan_id, task_id, attempt_id, outcome, summary, evidence, recorded_at
FROM task_result
WHERE plan_id = ? AND outcome = 'verified'
ORDER BY recorded_at, id;
