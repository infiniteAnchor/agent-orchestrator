# Phase 3: durable task graph execution plan

Phase 1 headless packaging and Phase 2 remote desktop support are complete.
Phase 3 is the durable task graph: validation, persistence, dispatch, and recovery.
The governing design is [Headless Server Control Plane](../headless-server-control-plane.md).

The daemon dispatches real worker sessions through the existing session and
isolated workspace services. Only explicit dispatch claims currently ready
tasks; recovery adopts or holds previously authorized attempts. Native provider
acceptance remains untested. See the
[runtime execution record](task-execution-runtime-gaps.md).

## Implementation slices

1. **Graph contract and validation (complete).** Add domain plan, phase,
   and task definitions. Reject missing identities/content, duplicate identities,
   invalid phase/dependency references, duplicate edges, self-dependencies,
   cycles, and missing verification on tasks that unlock dependents. Validate
   without invoking a harness or executing verification commands.
2. **Atomic persistence (complete).** Add new SQLite migrations, sqlc queries, and store
   methods for plans, phases, tasks, dependencies, attempts, and results. Validate
   graphs before atomic creation. Define attempt transitions and immutable result
   evidence before exposing mutations. Add trigger-backed task CDC vocabulary
   and replay tests. Test migrations against disposable databases; deploying a
   migration to existing user data is a separate operation.
3. **Service and API (complete).** Add project-scoped create/get/list operations using domain
   records behind a narrow store interface. Bound graph/request sizes, verify
   project ownership, and regenerate OpenAPI and frontend types. Define remote
   projections for result evidence before exposing server paths or command output.
4. **Ready queue and dispatch (complete).** Derive readiness from verified durable results;
   atomically claim tasks under project/harness concurrency limits. Persist an
   attempt identity before dispatch and carry it through runtime creation.
   Resolve workspace ownership conflicts before enabling parallel execution.
5. **Recovery and completion (complete).** Adopt existing work by attempt identity after a
   crash; hold ambiguous dispatches for reconciliation. Persist candidate results,
   verification evidence, and completion consistently. Recover collection and
   dependency readiness from SQLite. Gate dispatch during startup recovery.
   `/readyz` stays 503 with `taskRecovery=pending` until that recovery finishes;
   `/healthz` remains liveness. See the [contract index](phase-3-contract-index.md).

Each slice requires focused tests and diff review before the next slice. Keep
automatic dispatch disabled until claims, launch reconciliation, and verified
completion work together.

## Initial contract

- A plan belongs to one project and contains at least one task.
- Phase IDs and task IDs are unique within their respective plan collections.
  IDs are nonblank and cannot contain leading or trailing whitespace.
- Phases are optional grouping metadata. If a plan defines phases, each task
  references one of them. Phase ordering does not imply dependency edges.
- Dependencies reference tasks in the same plan, including across phases.
  Input order does not imply execution order.
- Every task has a title and prompt. Every supplied verification command is
  nonblank; any task with dependents must provide at least one command.
- Validation checks the declared verification requirement, not whether the
  command proves success. Executing and interpreting verification belongs to
  the later server-owned result collector.
- Harness selection, worktree policies, and retries remain with their consuming
  dispatch slices. Attempt and result records are durable internal contracts but
  are not exposed by the task-plan API; result evidence needs a dedicated
  remote-safe projection before it becomes a wire contract.

## Acceptance and remaining work

The completed contract accepts disconnected and out-of-order DAGs, rejects
invalid graphs deterministically, and handles deep dependency chains without
recursive cycle detection.

The scheduler claims a ready task only after writing its attempt and invokes
the launcher for that same attempt. It records a runtime ref when the launcher
reports a started worker. The current daemon launcher returns a logical
`attempt:<id>` ref without creating a runtime, worktree, or coding-agent session.
A verified result unlocks the next task while the first worker
may still be alive. Recovery adopts a remembered launch, holds an ambiguous
one, and rebuilds dependent readiness from SQLite. Clean process exit, Chat
`turn.completed`, and a failed or unknown runtime probe do not count as success
or as permission to start a replacement attempt. Phase 4 planner automation and
desktop task-plan review are implemented, but real launch/adoption and dispatch
authorization remain backend follow-ups. The index of these contracts is
[phase-3-contract-index.md](phase-3-contract-index.md).
