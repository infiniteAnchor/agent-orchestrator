# Phase 3 contract index

The durable task graph is specified in four layers. Later slices consume the
earlier contracts; they do not replace them.

| Contract | Where it lives | What it guarantees |
| --- | --- | --- |
| Graph | `backend/internal/domain/task_graph.go` | One project, unique phase and task ids, explicit edges, verification on tasks that unlock dependents, no stored "ready" state. |
| Attempt and result | `backend/internal/domain/task_execution.go` | Legal task and attempt transitions. Results are immutable. Only `verified` unlocks dependents. `inconclusive` holds work. |
| Schedule | `backend/internal/domain/task_schedule.go` | Readiness is derived from queued tasks plus verified results. Workspace conflicts, the project cap, and the harness cap fail independently. |
| Persistence | `backend/internal/storage/sqlite/migrations/0129_task_graph.sql`, `0130_task_schedule.sql` | Atomic plans. Compare-and-swap attempts. Append-only results. Trigger-backed `change_log` events. A dispatch lease is not a runtime identity. |
| Task-plan API | `POST/GET /api/v1/projects/{id}/task-plans` | Project-scoped create, get, and cursor list. Attempts and evidence are not on this wire. |
| Schedule API | `GET .../schedule`, `POST .../dispatch`, `POST .../tasks/{taskId}/candidate` | Opaque ids and derived state. No prompts, command output, or host paths. |
| Recovery | `backend/internal/service/tasksched` | Same attempt is adopted or held. A replacement attempt is not created when launch state is unknown. The default launcher records an attempt-scoped runtime ref; a launcher that creates a worker session returns that session id and the scheduler stores it on the attempt. |
| Readiness probe | `GET /readyz` | HTTP 200 only after task recovery finishes. `taskRecovery` is `pending` or `complete`. `GET /healthz` stays liveness and does not wait. |

Process exit and Chat `turn.completed` are rejected as completion signals.
Verification commands are executed by the server. A verified result makes
dependents ready even if the upstream worker is still alive.
