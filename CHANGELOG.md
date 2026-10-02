# Changelog

## [2026-10-02]

### Added

- Native Codex task-worker and Linux desktop acceptance evidence, including
  authenticated remote dispatch, verified dependency completion, and durable
  crash holds. Detached Chat reconnection and remaining platform/CI validation
  are explicitly recorded as gaps.

### Fixed

- Planner guidance now includes a valid task-graph example and explains phase
  identities, shell quoting, and independent worker workspaces.
- Updated Codex protocol generation and prevented discriminator-name collisions.
- Preserved omitted remote project paths across desktop forms, shell terminals,
  navigation, and shared project views.
- Bounded browser-history size fitting and stabilized lazy settings controls,
  installer cancellation, private credential fixtures, and desktop bridge tests.
- Cleared the recorded Go lint findings. Full frontend and ordinary backend
  tests pass; the SQLite migration race gate still exceeds its 15-minute limit.

## [2026-10-01]

### Added

- Desktop project task-plan review for local and remote daemons: bounded
  specification submission, proposal decisions, accepted-plan browsing,
  immediate dispatch requests, task/attempt progress, and human-gate decisions.
- Localized task-plan controls in all eight supported languages.

### Changed

- Task workers now launch through daemon-owned session and isolated workspace
  services, with atomic attempt/session ownership and crash-safe adoption or hold.
- New task claims require explicit plan dispatch. Acceptance, gate approval,
  background ticks, and restart do not activate plans. Verification uses the
  bound worker workspace and effective harness limits are enforced at claim time.

## [2026-09-22]

### Added

- Project-scoped planner proposals with durable Chat-turn recovery, bounded
  graph validation, explicit accept/reject operations, and atomic acceptance
  into the task-plan scheduler without direct dispatch by the acceptance
  handler. The automatic-claim gap found during desktop integration was closed
  on 2026-10-01; only explicit dispatch now claims new tasks.

## [2026-09-21]

### Added

- Phase 3 ready queue, attempt dispatch, verified completion, and crash
  recovery. A verified result unlocks dependents while the upstream worker may
  still be alive. Ambiguous launches are held instead of replaced.
- `GET /readyz` reports `taskRecovery` and stays unavailable until task
  recovery finishes. `GET /healthz` remains the liveness probe.
- Schedule, dispatch, and candidate routes on the task-plan API, plus the
  Phase 3 contract index.

## [2026-09-15]

### Added

- Durable Phase 3 task-graph persistence for plans, tasks, dependencies,
  attempts, immutable results, and trigger-backed CDC events.
- Bounded project-scoped task-plan create/get/list APIs with cursor pagination,
  ownership checks, LAN-safe projections, and generated TypeScript contracts.

## [2026-09-11]

### Added

- Initial Phase 3 task-plan domain model and dependency-graph validation;
  automatic scheduling remains pending.
- Phase 3 execution plan covering persistence, APIs, dispatch, and recovery.
