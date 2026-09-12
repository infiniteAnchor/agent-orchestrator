# Phase 3: durable task graph execution plan

Phase 1 headless packaging and Phase 2 remote desktop support are complete.
Phase 3 starts with a pure graph contract; it is not yet a runtime feature.
The governing design is [Headless Server Control Plane](../headless-server-control-plane.md).

## Implementation slices

1. **Graph contract and validation (initial slice).** Add domain plan, phase,
   and task definitions. Reject missing identities/content, duplicate identities,
   invalid phase/dependency references, duplicate edges, self-dependencies,
   cycles, and missing verification on tasks that unlock dependents. Validate
   without invoking a harness or executing verification commands.
2. **Atomic persistence.** Add new SQLite migrations, sqlc queries, and store
   methods for plans, phases, tasks, dependencies, attempts, and results. Validate
   graphs before atomic creation. Define attempt transitions and immutable result
   evidence before exposing mutations. Add trigger-backed task CDC vocabulary
   and replay tests. Test migrations against disposable databases; deploying a
   migration to existing user data is a separate operation.
3. **Service and API.** Add project-scoped create/get/list operations using domain
   records behind a narrow store interface. Bound graph/request sizes, verify
   project ownership, and regenerate OpenAPI and frontend types. Define remote
   projections for result evidence before exposing server paths or command output.
4. **Ready queue and dispatch.** Derive readiness from verified durable results;
   atomically claim tasks under project/harness concurrency limits. Persist an
   attempt identity before dispatch and carry it through session/runtime creation.
   Resolve workspace ownership conflicts before enabling parallel execution.
5. **Recovery and completion.** Adopt existing work by attempt identity after a
   crash; hold ambiguous dispatches for reconciliation. Persist candidate results,
   verification evidence, and completion consistently. Recover collection and
   dependency readiness from SQLite. Gate dispatch during startup recovery and
   define `/readyz` semantics explicitly.

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
- Harness selection, worktree policies, retries, attempts, and result records
  will be added with their consuming persistence/dispatch slices. The initial
  types are internal domain contracts, not an HTTP schema.

## Acceptance and remaining work

The initial slice must accept disconnected and out-of-order DAGs, reject invalid
graphs deterministically, and handle deep dependency chains without recursive
cycle detection. It introduces no database changes or runtime dispatch.

Phase 3 is complete only when two dependent tasks can run headlessly, a verified
result unlocks the second while the first worker may remain alive, and a restart
at the claim/launch/record boundary does not create duplicate work. Clean process
exit, Chat `turn.completed`, and failed runtime probes are insufficient evidence
for success or replacement. Planner automation remains Phase 4.
