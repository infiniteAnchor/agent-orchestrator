# Headless Server Control Plane

## Idea

Run Agent Orchestrator as a durable, headless service on a workstation, homelab
server, or VPS while keeping the Electron application as a remote control pane.
The desktop should connect over a trusted LAN or Tailscale network and provide
the Kanban, conversations, terminal views, reviews, and approvals. The server
should own repositories, worktrees, agent processes, credentials, logs, task
state, scheduling, and recovery.

This changes AO from a desktop-owned local daemon into a client/server product
without changing the core worker model:

```text
Electron desktop
  └── authenticated API + event stream
        └── headless AO server
              ├── project and task state
              ├── orchestrator/planner
              ├── dependency-aware scheduler
              ├── harness adapters
              ├── Git worktrees
              ├── worker processes
              └── durable events, logs, and artifacts
```

The desktop is a presentation and control client. It must not read the server's
filesystem, open the server's SQLite database, or assume that a terminal process
is local.

## Why

The intended workflow is larger than manually starting independent workers:

1. A strong orchestrator reads an existing specification, design document, or
   repository context.
2. It creates an implementation plan with phases, dependencies, acceptance
   criteria, and verification commands.
3. It slices the plan into focused worker tasks.
4. The scheduler routes each task to an appropriate harness/model based on
   capability, cost, current usage, and availability.
5. Independent tasks run in parallel worktrees; dependent tasks remain queued.
6. Worker completion, test results, CI feedback, and review comments become
   durable events.
7. The scheduler unlocks the next ready tasks and wakes the orchestrator only
   when a planning or review decision is required.

This keeps the expensive model event-driven. It does not need to remain in a
polling conversation while cheaper workers run, and a desktop disconnect must
not stop or lose work.

## Goals

- Run all execution unattended on a headless Linux server.
- Connect one or more desktop clients over LAN or Tailscale.
- Preserve AO's project orchestrator and isolated worker worktrees.
- Reuse the shipped agent-adapter registry (Codex, Claude Code, OpenCode, and
  the other compiled adapters). Named cost/capability profiles are new; there is
  no user-defined harness plugin loader today.
- Persist tasks, dependencies, worker attempts, results, artifacts, and events.
- Notify the scheduler when workers finish without relying on a connected UI.
- Parallelize tasks only when their dependency and workspace constraints allow it.
- Route work to cheaper models when the task does not require a strong model.
- Pause, retry, or reroute work when a provider is exhausted or unavailable.
- Reconcile active work after server, process, or network failure.
- Keep the current loopback daemon safe and preserve the opt-in LAN boundary.

## Non-goals for the first version

- Replacing the existing agent CLIs with a new model runtime.
- Making the desktop responsible for scheduling or process supervision.
- Public internet exposure or a hosted multi-tenant control plane.
- Carrying desktop event or terminal traffic over the Cloudflare quick tunnel
  used by Connect Mobile remote access (`docs/adr/0004-cloudflare-tunnel-for-remote-mobile-access.md`).
  That path buffers small SSE bodies and is not end-to-end encrypted.
- Automatically merging changes without the existing review and safety rules.
- Supporting arbitrary distributed workers across multiple servers in V1.

## Existing foundations

The daemon already owns execution and persistence independently of the renderer.
This proposal extends these existing capabilities:

- The hidden `ao daemon` command runs without Electron
  (`backend/internal/cli/root.go`). `ao start` is not a headless entry point; it
  fetches and opens the desktop app and no longer spawns the daemon.
- SIGINT/SIGTERM drive graceful shutdown (`backend/internal/daemon/daemon.go`).
  Startup reconciliation is split:
  - `ReconcileStartupSafety` runs **before** the listener binds and **fails boot**
    if active agent-switch / interface-transition sagas cannot be closed.
  - Persistent Chat host reconciliation, `ReconcileBackground`, and
    `ReconcileRuntime` run **after** the listener is up, in the background, and
    log failures rather than failing boot.
- `/healthz` and `/readyz` already exist (`backend/internal/httpd/router.go`).
  Both return a static payload once the process is listening. They do not wait
  for background reconciliation or inspect task recovery. Treating them as
  recovery-complete is new work.
- `/api/v1/events` replays durable CDC events from `change_log`, accepts `after`
  or `Last-Event-ID` cursors, and skips already-sent seqs
  (`backend/internal/httpd/events.go`). The CDC vocabulary today is session, PR,
  and review-run row changes (`backend/internal/cdc/event.go`). It does not
  carry Chat `turn.completed` or a task-attempt lifecycle.
- In local mode the renderer opens `/api/v1/events` with browser `EventSource` and
  `/mux` with browser `WebSocket` (`frontend/src/renderer/lib/event-transport.ts`
  and `frontend/src/renderer/lib/terminal-mux.ts`). Neither browser API can add
  the LAN `Authorization` header, so those direct transports cannot be reused by
  an authenticated remote desktop. The cloud client already demonstrates the
  relevant patterns: authenticated `fetch` streaming for SSE and a separately
  authorized terminal connection. Phase 2 now provides the remote desktop's
  main-process HTTP/SSE/mux bridges, described in the status section below.
- Connect Mobile already provides an opt-in authenticated LAN listener and an
  unauthenticated identity probe (`backend/internal/httpd/lan_listener.go` and
  `backend/internal/httpd/auth.go`). The terminal mux at `GET /mux` is on that
  same shared router, so an authenticated LAN client can already stream
  terminals. The mux currently accepts WebSocket upgrades with
  `InsecureSkipVerify` (no origin check).
- Tailscale Serve already fronts the authenticated LAN listener
  (`backend/internal/mobilebridge/tailscaleserve.go`). Cloudflare Tunnel is a
  separate Connect Mobile remote-access path in front of the same listener, not
  a third bind; do not use it for the desktop control plane.
- LAN auth today is one shared rotating connection password, compared
  constant-time, with per-source lockout after five failures (ADR 0001).
  Enable/disable/regenerate live on loopback-only `/api/v1/mobile/*`.
  `ao lan` is the non-Electron operator CLI for those routes; enable/regenerate
  post `lanOnly=true` so Cloudflare remote access is not started.
  `restoreMobileOnBoot` only rebinds when persisted state already says enabled
  and honors `lanOnly`.
- In local mode Electron discovers `running.json`, talks to
  `http://127.0.0.1`, and opens server-returned workspace paths with
  `shell.openPath`. `/api/v1/desktop` is LAN-blocked on purpose. Existing project
  responses and health probes retain host-local paths on loopback. Phase 2 now
  supports selecting an enrolled remote server and projects LAN responses
  through the remote-safe wire contract.

Service packaging, LAN enablement without Electron, and remote Electron
connection support are implemented in Phases 1 and 2. Remaining work is durable
task planning, scheduling, verification, and routing. Existing session
reconciliation and CDC replay are foundations to extend for task attempts; they do not already
implement the proposed task scheduler.

## Runtime modes

### Desktop-local mode

Keep the current behavior: the desktop starts a local daemon and connects over
the loopback listener. This remains the simplest default for local development.

### Headless mode

Package the existing `ao daemon` entry point for supported headless operation.
The public command name and service packaging can be decided during
implementation. Keep state under the configured AO data directory and retain
the loopback listener; remote access must explicitly enable the existing LAN
listener described below. Headless enablement cannot depend on the Connect
Mobile desktop UI: it needs a loopback/CLI/config path that writes the same
persisted LAN state `restoreMobileOnBoot` already understands.

The server should support systemd or another process supervisor. Build on
existing graceful shutdown and the **blocking** startup-safety reconciliation.
Do not treat `/readyz` as proof that Chat-host or runtime recovery has finished;
extend probe semantics only if task recovery must gate traffic.

Define service lifecycle ownership explicitly. The existing supervisor watchdog
listens on a Unix socket / named pipe next to `running.json`, not on HTTP. It
arms after its first client connects and can stop the daemon after the last
client disconnects (`backend/internal/daemon/supervisor/supervisor.go`). Remote
HTTP desktops do not keep that watchdog alive. A headless unit that never sees
a local Electron client already stays up; if a local Electron connects and then
exits, the daemon still stops after the grace period. Headless service lifetime
must ignore that callback (or never start the watchdog).

### Remote desktop mode

The desktop selects a server endpoint and authenticates to it. It uses the same
domain operations and event protocol as local mode, but all projects, worktrees,
terminals, logs, and agent processes are remote. This is not “point the typed
client at another host”: the main process must stop assuming loopback,
`running.json`, and local filesystem paths, and the shared wire shapes must be
audited before they are exposed remotely.

The Electron main process should own the remote endpoint and connection secret.
The renderer should call a narrow IPC-backed transport and receive only the
responses and events it needs; it should not hold the LAN bearer or construct
authenticated sockets directly.

## Server responsibilities

The server becomes the authority for:

- registered projects and repository locations;
- orchestrator sessions and worker sessions;
- task plans, phases, dependencies, and scheduling decisions;
- harness configuration and provider usage budgets;
- worktree creation, cleanup, and merge preparation;
- agent process spawning, output capture, cancellation, and reaping;
- test, CI, review, and merge observations;
- terminal and log streaming;
- durable events and reconnect/replay cursors;
- authentication, authorization, and audit information.

The desktop may cache read models for responsiveness, but any command that
changes server state must go through the authenticated API.

## Planning and scheduling model

Introduce a durable task graph above worker sessions. A task should contain at
least:

```text
id
project_id
phase_id
title and prompt
depends_on[]
preferred_harness
fallback_harnesses[]
cost/usage policy
worktree policy
verification commands
retry policy
status and attempt
artifacts and result summary
```

The orchestrator can produce this graph from a specification or existing
implementation document. The server validates the graph before execution:

- reject cycles;
- reject unknown dependencies;
- cap concurrency per project and harness;
- detect overlapping workspace ownership where possible;
- require verification for tasks that unlock other tasks;
- keep failed downstream work blocked rather than silently skipping it.

The ready queue is derived from durable task facts. Before dispatch, the
scheduler atomically claims a ready task and persists a stable attempt identity.
Worker launch or turn submission must use that identity for idempotency and
associate it with the resulting session, runtime, and provider turn identifiers.

A durable claim alone does not prevent duplicate workers: the server can crash
after launch but before recording the runtime identity. Recovery must locate and
adopt work by attempt identity or use an idempotent dispatch operation. If it
cannot establish whether dispatch succeeded, it must hold the attempt for
reconciliation rather than blindly launch another worker. Failed or unknown
runtime probes are not evidence that the original worker is dead. Persist
recovery decisions before allowing a replacement attempt to run.

## Event-driven completion

Task completion must be server-owned and durable. The task-attempt lifecycle is:

```text
queued → claimed → running → collecting →
  completed | failed | blocked | cancelled
```

Task completion, harness turn completion, and process termination are separate
facts. Persistent Chat sessions emit `turn.completed` on the **Chat conversation
bus** and settle turns without exiting
(`backend/internal/service/chat/controller.go`,
`ports.ChatEventTurnCompleted`). That event does **not** appear on
`GET /api/v1/events`. A completed turn may later show up as a CDC
`session_updated` (activity idle). The scheduler must not treat either as task
success.

A task can span multiple turns. A completed turn or clean process exit alone
does not prove that its acceptance criteria passed. Unlocking dependents
requires a new durable task-attempt fact (and a new `change_log` event type
emitted by a DB trigger), not a live Chat event or WebSocket message.

The server should collect a candidate task result when the harness reports it
through a turn result or explicit task-result signal. Process exit is another
observation to reconcile, including unexpected failure; it must not be the only
trigger for collection. The server should persist:

1. the harness result and relevant turn/process observations, tied to the attempt;
2. the final output and structured summary;
3. the diff, verification results, and produced artifacts;
4. the task state transition, marking success only after required verification;
5. a durable completion event with stable event/task/attempt identifiers;
6. readiness of dependent tasks derived from the verified durable result.

Collection must resume safely after a crash. Commit the result references and
state transition consistently, using the existing trigger-backed CDC boundary
for durable change events. Do not emit those events by hand from store methods.
Reconciliation must recover dependent-task readiness even if the server stops
before the scheduler observes the completion.

Event delivery to clients may be at-least-once. Clients must reconnect with a
cursor and tolerate duplicate events. The persisted task result, not a WebSocket
or Chat-bus message, is the source of truth.

The orchestrator should be notified through that durable CDC mechanism, not by
requiring a live MCP call, an in-process Chat subscription, or an active desktop
window. A planner/reviewer turn can then inspect the completed task and decide
whether to continue, revise, retry, or request human input.

## Cost and harness routing

Harnesses should be configured as named server-side profiles on top of the
existing compiled adapter registry
(`backend/internal/adapters/agent/registry/registry.go`). Profiles are new;
adapters are not user-loadable plugins:

```yaml
harnesses:
  codex-planner:
    command: codex
    role: planner-reviewer
    cost_class: high
    capabilities: [architecture, review, complex-implementation]

  opencode-worker:
    command: opencode
    role: worker
    cost_class: low
    capabilities: [implementation, tests, refactor]
```

Routing should consider task class, model context needs, current provider
usage, rate limits, and recent failures. When a provider is exhausted, the
server should preserve the worktree and task artifacts, record the reason, and
retry on a configured fallback with a handoff summary.

Provider credentials remain on the server and are never sent to the desktop
client. Codex account-management routes stay loopback-only; remote credential
setup is out of band (on-server CLI or SSH), not via the LAN desktop client.

## API and transport

Reuse the existing daemon's domain operations and typed contracts where their
wire shapes are remote-safe, including identity discovery and durable event
cursors. Extend their remote-client integration and add the missing contracts:

- authenticated server capability discovery as a **separate** route. Do not add
  fields to unauthenticated `GET /api/v1/identity`; ADR 0003 limits that body to
  `{ hostId, apiVersion }` (the mobile contract version);
- task/plan CRUD and command endpoints alongside existing project operations;
- task graph and scheduler state;
- task-attempt events as **new** `change_log` types on `GET /api/v1/events`,
  with the existing cursor replay. Do not overload Chat `turn.completed`;
- an Electron-main remote transport that attaches the bearer to bounded HTTP
  requests and streams authenticated SSE over `fetch`/`ReadableStream`, while
  preserving the last durable event cursor across reconnects;
- an Electron-main WebSocket connection to the existing `GET /mux`, using a
  client that can attach `Authorization` to the upgrade and forwarding bounded
  terminal frames over IPC. The browser `WebSocket` constructor cannot send
  that header. Origin checks on `/mux` are new; HTTP CORS already allowlists
  renderer origins;
- artifact and log download;
- explicit approvals, cancellation, retry, and merge commands;
- optional readiness semantics that wait for task recovery. Today's `/readyz`
  only means the process is listening.

Use HTTP for request/response operations, SSE for CDC, and WebSocket for
terminal streams. The protocol should work identically through localhost, LAN,
and Tailscale. It must not depend on the Cloudflare tunnel.

Before enabling the remote client, inventory every route it calls and define a
remote-safe projection for each response. In particular:

- do not return absolute project, worktree, transcript, executable, or working
  directory paths merely because the local DTO already contains them;
- represent remote resources with opaque project/session/artifact identifiers
  and server-executed actions;
- do not reuse the current `/healthz` and `/readyz` payloads remotely without
  removing their executable and working-directory fields, or introduce bounded
  remote probe responses;
- audit error details, logs, artifacts, and CDC payloads for the same boundary;
- keep local-only fields available on loopback when the desktop still needs
  them, rather than weakening local mode to obtain a shared shape.

## LAN and Tailscale security

The current loopback listener remains unchanged and unauthenticated. Extend and
reuse the existing explicitly enabled `LANManager` listener for remote desktop
connections, protected by the existing bearer-password middleware. Do not add a
third listener.

Preserve the complete LAN control-route filter in
`backend/internal/httpd/lan_listener.go`, not a summary of it. Today that is:

- prefix blocklist: `/shutdown`, `/internal/`, `/api/v1/mobile`, `/api/v1/dev`,
  `/api/v1/browser`, `/api/v1/desktop`, `/api/v1/system/install`,
  `/api/v1/agents/codex`;
- `POST /api/v1/agents/{id}/install`;
- `/api/v1/sessions/{id}/preview/server`.

Adding or removing a blocked path is a security change and needs tests next to
`TestLANManagerBlocksLoopbackOnlyControlRoutes`.

Preserve the sole authentication exemption: exact `GET /api/v1/identity`, checked
before authentication lockout. Other methods or related paths remain protected;
any additional unauthenticated route requires its own ADR. The probe must keep
returning only the opaque host id and mobile contract version.

For Tailscale deployments, reuse the existing Tailscale Serve integration in
front of the authenticated LAN listener. Do not enable Connect Mobile's
Cloudflare tunnel for desktop clients. Plain LAN mode should be treated as
home-network-only unless TLS is added.

Auth and authorization today are weaker than the target:

- one shared connection password for every LAN client; rotating it invalidates
  every client's next authenticated request, although already-upgraded
  WebSockets and open SSE responses are not proactively closed;
- HTTP CORS origin checks already exist; `/mux` does not check Origin;
- any authenticated LAN client can use the app API (minus the blocklist). There
  is no per-project authorization.

The server should grow toward:

- per-server enrollment or bearer tokens, with rotation and revocation that do
  not necessarily drop every other client;
- constant-time credential checks and rate limiting (already present for the
  shared password);
- origin checks for browser-based clients on `/mux` as well as HTTP;
- audit logs for mutating operations, correlated with the request IDs already
  installed by the HTTP middleware;
- explicit project-level authorization before terminal or file access.

The identity probe protects a credential only when its `hostId` is compared with
an identity learned through a trusted channel. Headless setup must print or
export the server's opaque host id separately from the connection secret. On
first enrollment, the user confirms that value in the desktop (or imports a
bootstrap artifact containing it); the desktop pins it to the saved server. On
every later connection it calls `GET /api/v1/identity`, compares the result
exactly with the pinned value, and sends no bearer on a mismatch. Merely
displaying or trusting whatever id the selected endpoint returns does not
prevent credential disclosure to a different machine.

## Implementation phases

### Phase 1: headless service packaging and lifecycle

- Package the existing `ao daemon` entry point for supported headless operation.
  Do not use `ao start`.
- Make service lifetime independent of the local Electron supervisor watchdog.
- Add a non-Electron way to enable the LAN listener and set or rotate the
  connection password (CLI or config that `restoreMobileOnBoot` honors).
- Add a headless enrollment command or bootstrap output that exposes the opaque
  host id and the connection secret as distinct values, so the desktop can pin
  identity before it sends the secret.
- Keep local mode working through the same daemon routes.
- Document that `/readyz` means “listening,” not “recovery finished.” Extend
  probe semantics only if task recovery must gate traffic.
- Document data directories, process ownership, and systemd deployment.

**Phase 1 status:** implemented in-tree. Operator packaging and systemd notes
live in [headless-deploy.md](headless-deploy.md) (`ao daemon --headless` /
`AO_HEADLESS`, data dirs, `/readyz`, `ao lan` with `lanOnly`, reference unit).
Later phases below are unchanged.

### Phase 2: remote client boundary

- Add endpoint selection to the desktop; stop discovering a local `running.json`
  and `127.0.0.1` when a remote server is selected.
- Enroll and persist an expected host id, consume `GET /api/v1/identity` before
  every authenticated connection, and fail closed without sending the bearer
  when it does not match. Put capability discovery on a separate authenticated
  route.
- Reuse the existing LAN listener, blocklist, and Tailscale Serve. Do not send
  desktop SSE or `/mux` through the Cloudflare tunnel.
- Audit the remote route set and add remote-safe projections that do not expose
  absolute host paths. Move filesystem actions behind daemon APIs that are
  allowed on LAN (`/api/v1/desktop` stays blocked).
- Keep the bearer in Electron main. Proxy bounded HTTP calls over IPC, consume
  authenticated SSE with `fetch`/`ReadableStream`, and open `/mux` with a
  main-process WebSocket client that can attach the bearer during upgrade.
- Add `/mux` origin checks and bounded IPC framing, and integrate reconnects with
  the existing durable event cursor without relying on browser `EventSource`.

**Phase 2 status (backend remote-safe wire): complete.** LAN requests are
marked via `reqctx.WithLAN` on the LAN listener, and the remote route set has
been audited. Absolute host paths are omitted on LAN for `/healthz`/`/readyz`
(`executablePath`, `workingDirectory`, `startupWorkingDirectory`,
`appImagePath`), project list/detail/initialize `path`, shell-terminal
`workingDir`, system requirement `detail`, agent installer plans and jobs
(`expectedDestination`, plus redacted `command`/`reason`/`output`/`error`),
model catalog `warning`, conversation activity `cwd` (dropped) and remaining
activity detail strings (redacted), and every `APIError` message and `details`
value. `internal/httpd/remotewire` owns the projection helpers; loopback keeps
the full shapes. The legacy import surface (`/api/v1/import`,
`/api/v1/imports/*`) is LAN-blocked: its inputs come from a desktop-native
folder picker, so a remote client cannot name a meaningful path, and it would
otherwise accept an arbitrary host path and run git there.

**Phase 2 status (desktop client boundary): complete.**
`frontend/src/main/remote-connection-store.ts`, `remote-identity-gate.ts`,
`remote-daemon-proxy.ts`, and `remote-mux-bridge.ts` pin the enrolled host id,
check `GET /api/v1/identity` before any authenticated call, keep the bearer in
the main process, proxy bounded HTTP over IPC (text bodies as UTF-8, binary
bodies base64 so bytes survive), stream authenticated SSE through
main's `fetch`/`ReadableStream`, and open `/mux` from a main-process WebSocket
client. The renderer now selects the connection: Settings → Remote servers
enrolls a server and switches between it and the local daemon; main reports the
remote target through the daemon-status handshake so `api-client`, the event
transports (`internal/httpd` SSE streams, notifications, workspace file
watches), and the terminal mux all rebind. Remote SSE resumes from the last
`id:` via the daemon's `after` cursor, and the main proxy allowlist mirrors the
LAN blocklist. Loopback-only surfaces are suppressed while remote: legacy
migration, the editor/file-manager handoff, Codex account management, Connect
Mobile, and harness installs.

### Phase 3: durable task graph

**Phase 3 status: started with the graph contract and validation slice.**
See the [execution plan](plans/phase-3-durable-task-graph.md) for the ordered
implementation and acceptance criteria. Persistence, API operations, dispatch,
and crash recovery remain pending; this is not yet a runtime scheduler.

- Persist plans, phases, dependencies, attempts, and task results.
- Add graph validation and a bounded ready-queue scheduler.
- Add durable attempt identities, idempotent dispatch or runtime adoption, and
  reconciliation for crashes between claim, launch, and runtime recording.
- Collect task results independently of process exit and of Chat
  `turn.completed`. Emit durable task lifecycle events through new
  trigger-backed `change_log` types.
- Unlock dependent work after verified completion.

### Phase 4: orchestrator automation

- Add planner prompts that produce validated task graphs.
- Add event-triggered planner/reviewer turns driven by durable task events, not
  a live Chat-bus subscription.
- Support handoff summaries and human approval gates.
- Add retry, fallback, and provider exhaustion policies.

### Phase 5: cost controls and operations

- Add per-project and per-harness budgets on top of existing usage collection.
- Add usage dashboards and forecast warnings.
- Add structured logs, metrics, and restart/recovery tests.
- Expand the Phase 1 reference systemd unit with a Tailscale deployment guide
  (the unit itself already lives in [headless-deploy.md](headless-deploy.md)).

## Success criteria

The first useful release should demonstrate that:

1. AO runs on a server with no desktop session or TTY, started via `ao daemon`
   (or the packaged service), with LAN enabled without Electron.
2. A desktop pins an out-of-band server identity, connects over Tailscale (not
   the Cloudflare tunnel), creates a plan, and sees live state through
   authenticated HTTP, SSE, and terminal transports without exposing the bearer
   to the renderer.
3. The orchestrator creates at least two dependent worker tasks.
4. Independent tasks run concurrently in isolated worktrees.
5. A verified durable task result unlocks the next task even when its persistent
   worker process remains alive; Chat `turn.completed` or process exit alone
   does not unlock dependent work.
6. Closing the desktop does not interrupt execution, including on a machine
   where the local supervisor watchdog would otherwise stop the daemon.
7. Restarting the server reconciles active tasks without duplication, including
   crashes after launch but before runtime recording; ambiguous attempts remain
   held until reconciled. Clients that connect at `/readyz` must not observe a
   duplicate dispatch while background recovery is still running.
8. A rate-limited worker can be retried on a fallback harness.
9. Provider credentials and absolute server-side paths remain private to the
   server; remote responses use opaque resource identifiers and remote-safe
   projections.
