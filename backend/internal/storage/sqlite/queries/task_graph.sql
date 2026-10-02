-- Task-graph persistence (slice 2 of the durable task graph). Writes are
-- composed by Store methods inside one transaction: a plan is validated by
-- domain.TaskPlan.Validate before any row is written, so an invalid graph never
-- reaches SQLite. Attempt transitions are compare-and-swap on the current state,
-- which is what makes a concurrent claim lose instead of double-dispatch.
--
-- Nothing here writes change_log. Task events are captured by the triggers in
-- migration 0129, and task_result is append-only at the schema level.

-- name: InsertTaskPlan :execrows
INSERT INTO task_plan (id, project_id, title, created_at, updated_at)
SELECT sqlc.arg(id), projects.id, sqlc.arg(title), sqlc.arg(created_at), sqlc.arg(updated_at)
FROM projects
WHERE projects.id = sqlc.arg(project_id) AND projects.archived_at IS NULL;

-- name: InsertTaskPhase :exec
INSERT INTO task_phase (plan_id, id, title, position)
VALUES (?, ?, ?, ?);

-- name: InsertTask :exec
INSERT INTO task (
    plan_id, id, phase_id, title, prompt, workspace_key, harness, position, state, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

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

-- name: ListTaskPlansPage :many
SELECT id, project_id, title, created_at, updated_at
FROM task_plan
WHERE project_id = sqlc.arg(project_id)
  AND (
    CAST(sqlc.arg(before_id) AS TEXT) = ''
    OR created_at < sqlc.arg(before_created_at)
    OR (created_at = sqlc.arg(before_created_at) AND id > CAST(sqlc.arg(before_id) AS TEXT))
  )
ORDER BY created_at DESC, id
LIMIT sqlc.arg(page_limit);

-- name: ListTaskPhases :many
SELECT plan_id, id, title, position
FROM task_phase
WHERE plan_id = ?
ORDER BY position;

-- name: ListTasks :many
SELECT plan_id, id, phase_id, title, prompt, workspace_key, harness, position, state, created_at, updated_at
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
SELECT plan_id, id, phase_id, title, prompt, workspace_key, harness, position, state, created_at, updated_at
FROM task
WHERE plan_id = ? AND id = ?;

-- name: TransitionTaskState :execrows
UPDATE task
SET state = ?, updated_at = ?
WHERE plan_id = ? AND id = ? AND state = ?;

-- name: InsertTaskAttempt :exec
INSERT INTO task_attempt (
    id, plan_id, task_id, attempt_number, state, session_id, harness, runtime_ref,
    claimed_at, started_at, finished_at, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetTaskAttempt :one
SELECT id, plan_id, task_id, attempt_number, state, session_id, harness, runtime_ref,
       claimed_at, started_at, finished_at, created_at, updated_at
FROM task_attempt
WHERE id = ?;

-- name: ListTaskAttempts :many
SELECT id, plan_id, task_id, attempt_number, state, session_id, harness, runtime_ref,
       claimed_at, started_at, finished_at, created_at, updated_at
FROM task_attempt
WHERE plan_id = ? AND task_id = ?
ORDER BY attempt_number;

-- name: ListOpenTaskAttempts :many
SELECT id, plan_id, task_id, attempt_number, state, session_id, harness, runtime_ref,
       claimed_at, started_at, finished_at, created_at, updated_at
FROM task_attempt
WHERE state IN ('claimed', 'running', 'collecting', 'blocked')
ORDER BY claimed_at, id;

-- name: NextTaskAttemptNumber :one
SELECT CAST(COALESCE(MAX(attempt_number), 0) + 1 AS INTEGER) AS attempt_number
FROM task_attempt
WHERE plan_id = ? AND task_id = ?;

-- name: TransitionTaskAttempt :execrows
UPDATE task_attempt
SET state = ?, session_id = ?, harness = ?, runtime_ref = ?, started_at = ?, finished_at = ?, updated_at = ?
WHERE id = ? AND state = ?;

-- name: ListAllTaskPlans :many
SELECT id, project_id, title, created_at, updated_at
FROM task_plan
ORDER BY created_at, id;

-- name: InsertTaskPlanProposal :execrows
INSERT INTO task_plan_proposal (
    id, project_id, request_key, specification, status, orchestrator_id, turn_id,
    graph_json, error_code, error_message, accepted_at, rejected_at, created_at, updated_at
) SELECT sqlc.arg(id), projects.id, sqlc.arg(request_key), sqlc.arg(specification),
         sqlc.arg(status), '', '', '', '', '', NULL, NULL, sqlc.arg(created_at), sqlc.arg(updated_at)
FROM projects
WHERE projects.id = sqlc.arg(project_id) AND projects.archived_at IS NULL
ON CONFLICT(project_id, request_key) DO NOTHING;

-- name: GetTaskPlanProposalByRequest :one
SELECT id, project_id, request_key, specification, status, orchestrator_id, turn_id,
       graph_json, error_code, error_message, accepted_at, rejected_at, created_at, updated_at
FROM task_plan_proposal
WHERE project_id = ? AND request_key = ?;

-- name: GetTaskPlanProposal :one
SELECT id, project_id, request_key, specification, status, orchestrator_id, turn_id,
       graph_json, error_code, error_message, accepted_at, rejected_at, created_at, updated_at
FROM task_plan_proposal
WHERE project_id = ? AND id = ?;

-- name: ListTaskPlanProposals :many
SELECT id, project_id, request_key, specification, status, orchestrator_id, turn_id,
       graph_json, error_code, error_message, accepted_at, rejected_at, created_at, updated_at
FROM task_plan_proposal
WHERE project_id = ?
ORDER BY created_at DESC, id
LIMIT ?;

-- name: SetTaskPlanProposalState :execrows
UPDATE task_plan_proposal
SET status = ?, orchestrator_id = ?, turn_id = ?, graph_json = ?,
    error_code = ?, error_message = ?, accepted_at = ?, rejected_at = ?, updated_at = ?
WHERE id = ? AND project_id = ? AND status = ?;

-- name: ListPendingTaskPlanProposals :many
SELECT id, project_id, request_key, specification, status, orchestrator_id, turn_id,
       graph_json, error_code, error_message, accepted_at, rejected_at, created_at, updated_at
FROM task_plan_proposal
WHERE status IN ('queued', 'generating')
ORDER BY created_at, id;

-- name: AcceptTaskPlanProposal :execrows
UPDATE task_plan_proposal
SET status = 'accepted', accepted_at = ?, updated_at = ?
WHERE id = ? AND project_id = ? AND status = 'ready';

-- name: RejectTaskPlanProposal :execrows
UPDATE task_plan_proposal
SET status = 'rejected', rejected_at = ?, updated_at = ?
WHERE id = ? AND project_id = ? AND status IN ('ready', 'invalid', 'failed');

-- name: ActiveProjectForPlan :one
SELECT task_plan.project_id
FROM task_plan
JOIN projects ON projects.id = task_plan.project_id
WHERE task_plan.id = ? AND projects.archived_at IS NULL;

-- name: LeaseTaskAttemptDispatch :execrows
UPDATE task_attempt
SET runtime_ref = 'dispatching', updated_at = ?
WHERE id = ? AND state = 'claimed' AND runtime_ref = '';

-- name: ReleaseTaskAttemptDispatch :execrows
UPDATE task_attempt
SET runtime_ref = '', updated_at = ?
WHERE id = ? AND state = 'claimed' AND runtime_ref = 'dispatching';

-- name: BindTaskAttemptRuntime :execrows
UPDATE task_attempt
SET runtime_ref = ?, session_id = ?, harness = ?, updated_at = ?
WHERE id = ? AND state = 'claimed' AND (runtime_ref = '' OR runtime_ref = 'dispatching');

-- name: ListProjectActiveTasks :many
SELECT task.plan_id, task.id, task.state, task.workspace_key,
       CAST(COALESCE((SELECT NULLIF(a.harness, '') FROM task_attempt a
                      WHERE a.plan_id = task.plan_id AND a.task_id = task.id
                        AND a.state IN ('claimed', 'running', 'collecting', 'blocked')
                      ORDER BY a.attempt_number DESC LIMIT 1), task.harness) AS TEXT) AS harness
FROM task
JOIN task_plan ON task_plan.id = task.plan_id
WHERE task_plan.project_id = ?
  AND task.state IN ('claimed', 'running', 'collecting', 'blocked');

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

-- name: BindTaskWorkerSession :execrows
UPDATE task_attempt
SET session_id = ?, harness = ?, updated_at = ?
WHERE id = ? AND state = 'claimed' AND runtime_ref = 'dispatching' AND session_id IS NULL;

-- name: IsTaskWorkerSession :one
SELECT EXISTS(SELECT 1 FROM task_attempt WHERE session_id = ?) AS is_task_worker;
