# Desktop task-plan review

Add a project-board Task plans dialog using the existing typed daemon client,
including remote desktop transport. Cloud projects and mock preview boards do
not expose daemon task-plan actions.

- [x] Submit bounded specifications with stable retry keys and review proposals.
- [x] Accept/reject proposals; keep dispatch a separate explicit action.
- [x] Browse accepted plans, dependencies, verification commands, schedule,
      attempt/session links, and human gates.
- [x] Refresh from durable events, with bounded polling for resources without CDC.
- [x] Validate behavior, localization, typecheck, renderer build, and diff review
      (repository validation gaps are recorded below).

The initial desktop work included no daemon scheduling changes. Its original
validation found automatic claims and a memory-only launcher in daemon wiring.
Those backend gaps are now closed by the subsequent runtime implementation:
acceptance and gate approval save facts, only explicit dispatch claims new work,
and real sessions use isolated workspaces with durable attempt ownership.
Desktop copy was updated in all eight languages. The original desktop validation
below is retained; new runtime validation and native acceptance limits are in
[task execution runtime gaps](task-execution-runtime-gaps.md) and the subsequent
[native acceptance record](task-execution-acceptance.md).

## Validation

- All 179 targeted tests pass. They cover proposal decisions, explicit dispatch
  requests, task-scoped
  gates, rejection confirmation, UTF-8 limits, retry keys, pagination, cache
  invalidation, delayed responses across remote-server switches, telemetry route
  normalization, localization, and existing board/topbar behavior.
- Renderer production build passes with the existing chunk-size warning.
- Browser smoke with mocked daemon responses verifies proposal acceptance,
  human-gate approval, and an explicit dispatch request; wide and narrow layouts
  were inspected. Native Electron/remote transport and AO Preview were not
  exercised: this environment has no active AO session and its Electron binary
  is unavailable.
- The complete frontend suite ran: 287 files passed and nine failed (3,844 tests
  passed, 17 failed, seven skipped). Failures are missing Electron/SQLite native
  bindings and missing landing dependencies (`cheerio`, `@ao/shared/constants`).
- Frontend typecheck reports the same eight existing optional-path type errors
  as an isolated HEAD baseline. No task-plan changes introduce type errors.
- Local validation used Node 22; frontend CI pins Node 24. No CI run or native
  packaging was performed for this change.

Two independent review rounds found and resolved task-scoped gate readiness and
late-response cache contamination defects. Final review reported no findings.

## Commit validation update

The combined desktop/runtime implementation was checked before the local commit:
287 frontend test files passed and nine failed (3,849 tests passed, 17 failed,
seven skipped). The failure classes match those above. Runtime validation and
remaining native acceptance limits are recorded in
[the execution record](task-execution-runtime-gaps.md). No release or push was
performed.

## Native acceptance update (2026-10-02)

The real Linux Electron checkout and preload passed local browsing and authenticated
remote task-plan browsing, explicit worker dispatch, verification, and repeated
dispatch with no duplicate claims. The remote test used the isolated daemon's
LAN listener on the same host. Wide and narrow native screenshots are in the
[acceptance evidence](../screenshots/task-execution-acceptance/README.md).

The recorded optional-path errors and fixture/dependency failures are resolved.
With Node 24.21.0, the complete frontend suite passed 296 files and 4,034 tests
(six skipped); frontend/E2E typechecks, renderer build, and all 57 renderer smoke
tests passed. Product UI typecheck, 125 tests, build, and pack dry-run passed.

Backend build, vet, lint, and ordinary tests passed. The SQLite migration race
package exceeded the 15-minute limit in both the full run and an isolated rerun;
that validation gate remains red. Crash recovery proved durable conservative
holds, with seamless detached Chat adoption still unverified. Native macOS/Windows
packaging, a second physical machine, and remote CI remain unchecked. See the
[acceptance record](task-execution-acceptance.md) for complete limits and cleanup.
