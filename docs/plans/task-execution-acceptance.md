# Native task execution acceptance

Follow-up to the runtime implementation, authorized on 2026-10-01. Use a
disposable Git project and isolated AO state under `~/.ao/dev/task-acceptance`.
Do not migrate existing user data, publish, or push.

- [x] Generate and accept a native planner proposal; confirm acceptance stays inert.
- [x] Explicitly dispatch an installed-harness worker in an isolated worktree.
- [x] Verify its result and observe dependent readiness without automatic launch.
- [x] Restart the daemon and check durable identity, adoption/holds, and no duplicates.
- [x] Exercise native desktop task-plan and remote transport flows.
- [x] Resolve recorded backend/frontend validation failures and run full affected gates
      (SQLite migration race timeout resolved by the follow-up below).
- [x] Review the final diff, record evidence and limitations, and clean up test processes.

## Native results (2026-10-02)

The checkout's compiled daemon ran with disposable state and a fresh Git project.
Installed Codex 0.160.0, already authenticated on this machine, used `gpt-6-luna`
for bounded planner and worker turns. No existing AO database was migrated.

- A native planner returned a graph with two dependent tasks. Acceptance created
  no attempts, including after background ticks. Explicit dispatch created one
  real Chat worker session with its own worktree and durable attempt binding.
- The first planner output nested tasks inside phases and was correctly rejected.
  The prompt now supplies a valid, tested API example and explains phase identity,
  top-level tasks, shell command encoding, and independent worktrees. Subsequent
  native proposals passed graph validation.
- A worker wrote `marker.txt`; server-owned verification in that worker's
  directory passed. Its dependent became ready without launching automatically.
  A held attempt from the crash test correctly prevented a separate task using
  the same workspace key from dispatching. Explicit independent keys allowed
  the completion plan to proceed under the normal scheduler rules.
- SIGKILL of the isolated daemon during a real worker turn preserved the attempt
  and session ids. Recovery held the attempt as blocked, with no new session or
  duplicate dispatch. **Detached Chat adoption was not established:** the startup
  task guard does not reconnect an absent Chat controller. This acceptance proves
  the conservative hold path, not seamless continuation after a daemon crash.
- The real Electron checkout was built and launched through Forge under Xvfb,
  then restarted under Playwright while retaining Forge's renderer server. The
  real preload and daemon were used. Local task-plan browsing and authenticated
  remote browsing passed. Enrollment pinned the isolated server's host id; the
  public profile omitted its password. The remote dialog displayed verified
  completion, dependency readiness, and real worker-session links.
  [Screenshots](../screenshots/task-execution-acceptance/README.md) show the local
  view and the completed remote plan at wide and narrow window sizes.
- Clicking the remote dialog's dispatch action launched the dependent worker.
  Its own-worktree verification passed through the native remote bridge. Repeated
  dispatch returned zero claims. Both tasks now have completed durable attempts.

## Validation fixes and checks

- Regenerated the Codex wire vocabulary from the installed provider. The generator
  now resolves normalized discriminator-name collisions, including collisions
  with already-suffixed names. Focused generator and planner race tests passed.
- Credential test fixtures explicitly protect their disposable directories;
  production account safety checks are unchanged. Installer cancellation testing
  now waits for execution to start before canceling rather than racing a download.
- Fixed all 27 recorded Go lint findings. Pinned golangci-lint v2.12.2 reports zero
  issues. Go build and vet pass using workspace-required Go 1.26.5. The module's
  1.25.7 toolchain alone cannot load this repository's Go 1.26.5 workspace.
- Full backend `go test ./...` passed. The full Go 1.26.5 race run passed every
  package except the SQLite migration package, which exceeded the CI command's
  15-minute limit. An isolated rerun of that package also exceeded 15 minutes
  during a different migration test; this initial migration race gate was red
  locally and is resolved by the follow-up below.
  The cancellation-test failure observed under concurrent load
  was fixed and its focused race test passed ten repetitions.
- All eight recorded frontend optional-path type errors are fixed while preserving
  remote path omission. E2E bridge fixtures now match the real bridge interface.
  History-file fitting uses bounded binary search with an exact size-boundary
  regression test. Settings tests await their lazy-loaded controls.
- Under Node 24.21.0, the full frontend suite passed: 296 files, 4,034 tests, six
  skipped. Frontend and E2E typechecks and renderer production build passed.
  Renderer smoke passed all 57 tests after installing the missing Chromium binary.
  Product UI typecheck, all 125 tests, build, and pack dry-run passed.
- OpenAPI and pinned TypeScript schema regeneration produced no drift. Final
  review found and fixed the generator suffix collision; diff whitespace is clean.
  An independent review worker was unavailable after hitting its usage limit, so
  the coordinator reviewed the diff directly.

Native Windows/macOS packaging, remote-machine networking, and release/CI checks
were not run. The remote desktop test used the real authenticated LAN listener
on the same Linux host, not a second physical machine. No publish or push was run.

All acceptance controllers and daemons were stopped, including the orphaned
crash-test host. The test LAN listener is disabled. Dirty test worktrees and
private isolated state were retained under `~/.ao/dev/task-acceptance` rather than
force-deleted. The development desktop was restarted for main/preload changes;
its renderer ran at `http://localhost:5173`, its daemon at `127.0.0.1:43122`, and
the separate task daemon at `127.0.0.1:43121`. No test Electron/Xvfb process remains.

## Migration race validation follow-up (2026-10-02)

The timeout came from repeatedly rebuilding empty historical schemas under the
race detector. Upgrade-test setup now clones cached, immutable historical
snapshots into separate files. Each snapshot is checkpointed before copying and
restores its connection-local foreign-key setting. Seeded upgrades, repairs,
rollbacks, and fresh-install tests still execute real migrations. A regression
test verifies historical version identity, foreign-key enforcement, and clone
isolation. Independent review found no defects.

The complete Go 1.26.5 `go test -race -timeout=15m ./...` passed. The SQLite
migration package finished in 685.334 seconds, compared with the previous
900-second timeouts and an archived 889.568-second isolated pass. The SQLite
store race package passed in 498.925 seconds.

Build, vet, formatting, and golangci-lint v2.12.2 (zero findings) passed. Linux
CLI E2E and the Docker fresh-install smoke check passed. API regeneration showed
no OpenAPI or TypeScript drift; the cloud client regenerated without drift and
passed typecheck, all 21 tests, and pack dry-run. The earlier full frontend,
renderer smoke, and product UI results remain applicable because those sources
and dependencies are unchanged by this test-only follow-up. Native macOS/Windows
checks require CI runners; no release or publishing step was used for validation.
