# Task execution runtime gaps

Recorded during desktop task-plan integration on 2026-10-01. The findings below
are historical evidence. Both implementation gaps are now closed alongside
the desktop task-plan work. Native installed-provider acceptance
was subsequently exercised; see [native acceptance](task-execution-acceptance.md)
for completion, desktop transport, crash holds, and remaining validation limits.

## Completed desktop work

Project boards provide Task plans for local and enrolled remote daemon projects:

- Submit specifications bounded to 32,768 UTF-8 bytes with stable retry keys.
- Review task graphs, dependencies, prompts, and verification commands; accept
  ready proposals and reject ready, invalid, or failed proposals.
- Browse accepted plans with pagination, request immediate dispatch, inspect
  task/attempt state, and follow worker-session links when the server supplies them.
- Approve human gates or confirm rejection. A pending gate blocks only its
  task, allowing independent ready tasks to remain dispatchable.
- Refresh from task CDC, with mounted polling for proposals, gates, recovery,
  and active task schedules. Fence mutation responses across server changes.
- Use localized controls in all eight languages and normalize task resource
  identities out of API telemetry. Cloud and mock preview boards are excluded.

Validation: 179 targeted tests, a renderer production build, and a browser
smoke check with mocked daemon responses passed. Two review rounds resolved
gate readiness and late-response cache defects. The full suite had dependency
failures, and typecheck had eight errors matching an isolated HEAD baseline.
Native desktop/remote transport was not exercised. See the
[desktop execution record](desktop-task-plan-review.md) for exact results.
These changes are included with the runtime implementation and have not been
release-verified.

## Original finding 1: no real task-worker launcher

Evidence:

- [Daemon wiring](../../backend/internal/daemon/daemon.go) constructs the scheduler
  with `tasksched.NewMemoryLauncher()`.
- [MemoryLauncher.Dispatch](../../backend/internal/service/tasksched/launcher.go)
  returns `LaunchStarted` and a logical `attempt:<id>` reference. It does not
  create a coding-agent session, runtime, or isolated worktree.
- Its launch registry is process-local. After restart an unremembered attempt
  is ambiguous; it cannot discover or adopt a real surviving worker.

The API can therefore show a logical running attempt without an actual worker.
Domain, store, and scheduler tests establish contracts, not a complete native
execution path. The next launcher must use the existing daemon-owned session
and workspace boundaries, preserve attempt identity, record a real session id,
and adopt or hold work after crashes without duplicate launches.

## Original finding 2: acceptance is eligible for automatic dispatch

Evidence:

- [Scheduler.Run](../../backend/internal/service/tasksched/scheduler.go) recovers
  startup state, then calls `DispatchAll` every two seconds.
- Accepting a proposal creates a task plan visible to that loop. Approving a
  gate can make its task claimable on a later tick.
- Neither handler calls dispatch directly, but neither prevents background
  claims. The documented explicit-dispatch-only acceptance check is unmet.

Graph readiness and human-gate approval are not sufficient evidence of explicit
launch authorization. Before binding a real launcher, enforce the documented
requirement that accepted work remains inert until an explicit dispatch request.
Do not silently add durable automatic plan activation; that would require a
separate, documented contract for consent, dependency progression, and restart.
The current desktop copy accurately describes the existing automatic scheduler
and must be updated when the backend behavior changes.

## Implementation order and acceptance

1. Enforce explicit dispatch authorization before connecting real worker launch.
   Recovery must continue to adopt/hold existing attempts without claiming new
   ready tasks. Acceptance, gate approval, idle ticks, and restart must not
   create new attempts by themselves.
2. Replace the daemon's default in-memory launcher with an adapter through the
   existing session/workspace services. Persist launch identity before side
   effects and reconcile crashes before/after session creation and launch recording.
3. Test explicit dispatch, repeated dispatch, task-scoped gates, workspace and
   concurrency limits, verified dependency readiness, and crash recovery with
   fakes/disposable stores. Unknown runtime state must hold work, never prove absence.
4. Run relevant backend and API drift gates. Update desktop copy and tests to
   the resulting contract. Report native acceptance gaps without claiming that
   mocked tests demonstrate a real installed harness completing work.

Do not run migrations against existing user data or launch paid provider work
as incidental validation. Preserve the existing desktop implementation and
unrelated working-tree changes; commit/push only when authorized.

## Runtime implementation record (2026-10-01)

- `Scheduler.Run` recovers on startup and ticks without claiming new tasks.
  Each explicit dispatch claims only currently ready tasks under existing
  task-scoped gate, verified dependency, workspace, and concurrency checks.
  Acceptance and gate approval do not activate plans. Dispatch again when
  dependencies complete, gates open, or capacity becomes available.
- `SessionLauncher` replaces the daemon's default memory launcher. The existing
  session service resolves configuration and creates isolated worker workspaces.
  Session allocation and the attempt's session association commit in one SQLite
  transaction before workspace/controller side effects. Claims pin the effective
  worker harness so default and explicit selections share the correct capacity.
- Recovery uses the same durable attempt/session identity. A leased attempt with
  no session association can safely resume creation; a surviving observable
  worker is adopted. Partial seeds, missing workers, unknown runtime probes,
  and lost launch-record writes hold the original attempt without replacement.
  Held attempts stay held for inspection; this change adds no automatic repair
  or plan activation. Legacy logical memory-launch attempts become held.
- Session startup recovery runs before task recovery. Both live reconciliation
  and shutdown-saved restoration observe task-bound workers without relaunching
  controllers or resending prompts. A missing Chat controller is inconclusive;
  task recovery does not resume a provider conversation to manufacture liveness.
- Candidate verification runs in the bound worker workspace. Missing workspace
  resolution records an inconclusive result rather than checking the project
  checkout. Process exit and Chat turn completion remain insufficient evidence.
- Desktop acceptance/dispatch hints and their tests now describe explicit
  dispatch in all eight supported languages.

Validation uses fake providers/runtimes and disposable SQLite stores. No paid
provider work or existing-user-data migrations were performed. The completed
implementation is committed locally at the user’s request; it has not been pushed.
Independent review found and fixed effective-harness capacity accounting and
session startup relaunch bypasses; final review reported no findings.

### Validation

Passed:

- Full task scheduler, task automation, task-plan, session service, session
  manager, daemon, SQLite migration/store, and HTTP/API tests. The final
  `go test ./...` completed with only the three existing failing packages below.
- Race checks covering task-worker creation, capacity pinning, session startup
  holds, explicit dispatch, crash boundaries, and launch-record failures. The
  full task scheduler race suite also passed.
- `go build ./...` and `go vet ./...`; affected manager/store vet after the
  final startup recovery guard.
- OpenAPI spec regeneration and pinned `openapi-typescript@7.4.4` generation
  produced no drift. The root `npm run api` initially lacked the installed
  executable; the documented equivalent pinned generation completed.
- 85 targeted desktop tests across the dialog, hooks, API client, and event
  transport; the 11 dialog tests passed again after adding explicit-copy
  assertions. Renderer production build and final diff whitespace checks.

Existing failures, separate from this change:

- Full backend test failures in `codexappserver` (installed provider protocol
  differs from the generated protocol), `integration` (Codex bootstrap reports
  `account_storage_unsafe`), and `service/agent` (private credential fixtures
  rejected for a writable ancestor). All three failure classes reproduced in
  disposable untouched-HEAD archives. No generated provider protocol or account
  security behavior was changed to mask them.
- Pinned golangci-lint reports 27 existing issues in task-automation/planner,
  scheduler, domain, and SQLite helpers. New worker-launch/binding code has no
  lint findings. All 27 reported code snippets match unchanged HEAD code; the
  six scheduler findings also reproduced under pinned lint on an isolated HEAD
  archive. Full archived lint could not load its package set, so no independent
  full-baseline count is claimed. The full working-tree lint gate remains red.
- The pre-commit full frontend suite finished with 287 files passing and nine
  failing: 3,849 tests passed, 17 failed, and seven were skipped. Failures match
  the earlier desktop validation: missing Electron/SQLite native bindings and
  landing dependencies (`cheerio`, `@ao/shared/constants`).
- Frontend typecheck still reports the eight optional host-path errors recorded
  against the earlier desktop HEAD baseline: `ProjectSettingsForm`,
  `useAgentAuth`, `useShellTerminals`, `useSystemRequirementsGate`,
  `useWorkspaceQuery`, and `_shell`.

Native installed-harness completion, detached provider recovery, and desktop
transport acceptance were not exercised. Mocked/fake tests do not establish that
an installed paid provider completes a task. No release/publish jobs were run.
