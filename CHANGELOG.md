# Changelog

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
