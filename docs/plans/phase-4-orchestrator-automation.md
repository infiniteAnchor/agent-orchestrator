# Phase 4: orchestrator automation

Phase 3 provides validated task graphs, durable attempts and results, a derived
ready queue, and crash recovery. Phase 4 adds the orchestrator that proposes
and manages that work. Durable task facts remain the source of truth; planner
automation must not depend on a live Chat-bus subscription or an open desktop.
See the [control-plane design](../headless-server-control-plane.md) and the
[Phase 3 contract index](phase-3-contract-index.md).

## Implementation slices

1. **Planner proposal and approval (implemented).** Accept a bounded project
   specification, ask a configured planner to produce a task graph, validate
   it with `domain.TaskPlan.Validate`, and persist it as a draft proposal.
   The user can inspect, accept, or reject the proposal. Accepting atomically
   creates the existing durable task plan; proposal generation never dispatches
   work. Persist enough request/result state to survive daemon restart and make
   create/accept retries idempotent. Keep planner output within the current
   task graph contract; harness fallback and retry policy are not planner
   output in this slice.
2. **Durable event-driven planner/reviewer turns.** Consume task lifecycle CDC
   events using a durable cursor/claim so a restart can resume without missing
   or double-applying work. Trigger follow-up review/planning from persisted
   task results, not `turn.completed` or an in-process Chat-bus subscription.
   Make event handling idempotent and bound how many follow-up turns one event
   can create.
3. **Handoff summaries and human gates.** Store a bounded, task-linked summary
   for continuation/review. Add explicit approval state and commands for work
   that needs a person, with durable notifications and restart-safe resolution.
4. **Retry, fallback, and exhaustion policy.** Add durable policy and
   transitions for retryable failures, configured fallback harnesses, provider
   exhaustion, and terminal escalation. Preserve attempt identity, worktree,
   and artifacts; never retry an ambiguous launch as though it failed.

Each slice needs focused domain/service/store/controller tests and a diff review
before the next slice. Automated dispatch remains governed by Phase 3 recovery
and readiness contracts.

## First slice contract

### User flow

1. A user submits a project-scoped specification and requests a planner
   proposal.
2. The daemon records the request before starting the planner turn, then stores
   the returned candidate graph and validation outcome.
3. The user fetches the proposal, reviews tasks/dependencies/prompts, and
   accepts or rejects it.
4. Acceptance creates one normal task plan atomically and idempotently. The
   existing explicit dispatch operation remains a separate action.

### Boundaries

- Proposal APIs are authenticated and project-scoped. The project must be
  active, and the selected planner must be available through a server-owned
  harness configuration. Provider credentials stay on the server.
- Bound specification size, planner output size, task count, task text, and
  generation duration. Treat planner output as untrusted input and validate
  with the existing graph validator before it can be accepted.
- Persist proposal states sufficient to distinguish queued/running, ready for
  review, invalid/failed, accepted, and rejected. State changes use
  compare-and-swap semantics; repeating acceptance returns the same task-plan
  identity and cannot create a duplicate plan.
- Use the existing project-scoped Chat orchestrator and its configured
  permission policy. If the active orchestrator is not in Chat mode, fail the
  proposal with a safe status instead of silently changing its mode. The API
  is the first client surface; a dedicated desktop review flow can consume it
  in a later slice.
- A proposal is not a task plan and cannot enter the ready queue. Acceptance
  is the only operation that turns its validated graph into an executable
  plan. Rejecting or failing proposal generation never schedules workers.
- Keep proposal DTOs remote-safe: opaque identifiers, no absolute paths,
  credentials, raw provider diagnostics, or unbounded prompt/result bodies.
  User-authored specifications and task prompts are returned only through
  authenticated project APIs with explicit size limits.
- Use the existing Go DTO → OpenAPI → TypeScript generation flow for new API
  contracts. Preserve the CLI as a thin daemon client.

### Work areas

- Inspect existing project-scoped task-plan service/store and task-plan
  migrations before defining proposal persistence. Add a new migration; never
  edit merged migrations.
- Define proposal domain states and idempotent transitions, a narrow planner
  port, storage queries, service operations, and authenticated HTTP endpoints.
- Wire the first concrete planner through existing daemon-owned agent/Chat
  capabilities where possible. Do not add a second provider runtime or route
  planner control through the browser renderer.
- Add proposal create/get/list/accept/reject behavior and API drift updates.
  A proposal retry key must be scoped to its project and must not permit
  accepting different graph content under an already accepted identity.
- Document operator configuration for the planner, including the unavailable
  provider and invalid-output paths.

### Acceptance checks

- A valid planner graph is stored and can be fetched after daemon restart.
- Invalid or oversized output is retained as a bounded failure outcome and
  cannot become a task plan.
- Accepting a proposal creates exactly one task plan; retrying the same accept
  returns that plan. A second, conflicting acceptance is rejected.
- A rejected proposal cannot be accepted or dispatched.
- Proposal creation, planner failure, approval, and process restart do not
  dispatch workers. Only the existing explicit dispatch route does so.
- API tests prove active-project ownership, error-envelope behavior, and LAN
  response safety; generated OpenAPI and TypeScript artifacts are current.

The implementation exposes `POST/GET /api/v1/projects/{id}/task-plan-proposals`,
proposal detail, and explicit `accept` / `reject` actions. Planner work resumes
after session restoration on daemon startup; it reuses the durable Chat message
id if a crash occurs after turn submission. Proposal generation has a 15-minute
deadline and records a safe failure state on timeout.

## Decisions to settle during slice 1 design

- Whether the first planner turn uses a persistent project orchestrator Chat
  session or a dedicated planner invocation API. Prefer the existing
  daemon-owned Chat lifecycle if it can return bounded structured output
  without coupling proposal durability to one provider's transcript format.
- Whether invalid model output is repairable in the same proposal or requires
  a new proposal revision. The initial implementation should favor a new
  revision unless a bounded repair loop has clear idempotency and cost limits.
- Whether proposal retention is permanent or time-bounded. Rejected and
  superseded proposals must not be silently deleted before their disposition
  is visible to the user.
