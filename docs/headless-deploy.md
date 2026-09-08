# Headless daemon deployment

Operator runbook for running the AO backend as a long-lived service without the
Electron supervisor. This is Phase 1 packaging only: the daemon still owns
loopback discovery via `running.json`, and remote desktop enrollment is not
finished. For the broader design, see
[headless-server-control-plane.md](headless-server-control-plane.md).

## When to use headless vs desktop-local

| Mode | Use when | How the process stays up |
| ---- | -------- | ------------------------ |
| Desktop-local | Day-to-day work on the same machine as the UI | Desktop app owns the daemon. `ao start` fetches/opens the desktop app; it does **not** spawn the daemon. |
| Headless | Always-on host (CI box, lab server, shared machine) with no local Electron lifetime | Run `ao daemon --headless` (or systemd with `AO_HEADLESS=on`). Skips the Electron supervisor watchdog so a local app connect/quit cannot stop the service. |

Keep desktop-local as the default. Use headless only when you need the daemon
to outlive any local Electron client.

## Start the daemon

Hidden entrypoint (not shown in `ao --help`):

```bash
ao daemon --headless
```

`--headless` sets `AO_HEADLESS=on` before config load. Equivalent for systemd or
other supervisors that prefer env alone:

```bash
AO_HEADLESS=on ao daemon
```

Accepted toggle values: `on` / `off` (also `true`/`false`, `1`/`0`, `yes`/`no`).
Default is off: frontend-death auto-stop remains enabled.

The process binds loopback `127.0.0.1` (default port `3001`, overridable with
`AO_PORT`), writes `running.json`, and blocks until SIGINT/SIGTERM or
`POST /shutdown` from loopback.

Product CLI commands (`ao agent ls`, `ao spawn`, `ao lan …`, and so on) still
discover the daemon through `running.json` on loopback. If nothing is listening,
they fail with the existing “daemon is not running — start it with `ao start`”
message. That wording is unchanged; on a headless host, start
`ao daemon --headless` (or the unit below) first instead of `ao start`.

## Data directories and process user

Canonical AO home is `~/.ao` for the **process user**. That user must own the
tree; do not run the daemon as root while pointing state at another account’s
home.

| Path | Default | Override |
| ---- | ------- | -------- |
| AO home / state root | `~/.ao` | When `AO_DATA_DIR` is set, that path is also the state root |
| SQLite data dir | `~/.ao/data` | `AO_DATA_DIR` |
| Run file | `~/.ao/running.json` | `AO_RUN_FILE` |
| LAN / mobile config | `~/.ao/data/mobile/config.json` | under `AO_DATA_DIR` (`mobilebridge.Path`) |
| Host identity | `~/.ao/data/mobile/identity.json` | under `AO_DATA_DIR` (`mobilebridge.IdentityPath`) |

Confirm in code: `config.Load` resolves `RunFilePath` / `DataDir` / `StateDir`;
`daemon` wires `mobilebridge.Path(cfg.DataDir)` and
`mobilebridge.EnsureLocalIdentity(cfg.DataDir)`.

If you set `AO_DATA_DIR` for an isolated unit, also set `AO_RUN_FILE` under the
same tree so CLI discovery and the daemon agree. Example:

```bash
export AO_DATA_DIR=/var/lib/ao
export AO_RUN_FILE=/var/lib/ao/running.json
export AO_HEADLESS=on
```

## Readiness probes

`GET /healthz` and `GET /readyz` return the same static probe payload once the
HTTP server is listening (`daemonProbePayload` in
`backend/internal/httpd/router.go`). They report process metadata (status,
service name, pid, paths). They do **not** wait for background Chat-host or
runtime reconciliation.

Before bind, boot still runs blocking `ReconcileStartupSafety`. If that fails,
the daemon exits and never listens. After listen, Chat-host / session /
runtime reconciliation continues in the background. Treat `/readyz` as
“listening,” not “recovery finished.”

## Opt-in LAN listener

Loopback stays `127.0.0.1` and unauthenticated. Remote reachability uses the
existing Connect Mobile LAN path:

- Binds `0.0.0.0` (default port `3011`) **only while enabled**
- Bearer password auth on the app API
- Unauthenticated identity probe: `GET /api/v1/identity` → `{hostId, apiVersion}` only

Enable **after** the daemon is up, from a shell that can reach loopback:

```bash
ao lan enable
# or: ao lan enable --json
```

`ao lan` talks only to loopback
`/api/v1/mobile/{status,enable,disable,regenerate}`. Enable and regenerate send
`{"lanOnly":true}` so the daemon starts the `0.0.0.0` listener **without** the
Cloudflare remote-access connector (and stops a connector if one was already
running). That preference is persisted and honored by `restoreMobileOnBoot`.
`ao lan` does not configure Tailscale Serve.

Output prints **host id** and **password** as distinct fields. When a password
is present, the CLI reminds you to pin the host id in the desktop **before**
sending the password. Do not share or type the password until the client has
pinned that host id (or you are deliberately using another trusted out-of-band
check).

```bash
ao lan status          # enabled?, host id, bound host/port, tailscale host if any
ao lan regenerate      # rotate password; pin host id again before distributing
ao lan disable         # tear down the 0.0.0.0 listener
```

### What not to enable for desktop control-plane access

Do **not** use Connect Mobile’s Cloudflare / remote-access tunnel as the path
for a desktop control plane. Phase 1 remote access for operators is the
existing LAN listener (optionally reached through your own overlay).

### Tailscale

Tailscale (or similar) may sit in front of the LAN listener as an optional
overlay. `ao lan` does not configure Tailscale Serve or MagicDNS; bring that up
with your normal Tailscale tooling if you want it.

## Signals and stop

- SIGINT / SIGTERM: graceful shutdown (same path as loopback `POST /shutdown`)
- `AO_SHUTDOWN_TIMEOUT` caps graceful shutdown (default `10s`)
- Sessions are not torn down on daemon exit; the next boot’s reconcile adopts them

`ao stop` still works over loopback when `running.json` points at this process.

## systemd reference

One reference unit (inline). Adjust `User`, `Group`, and `ExecStart` for the
host. `Type=simple`: the daemon does not implement `sd_notify`.

```ini
[Unit]
Description=Agent Orchestrator headless daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=ao
Group=ao
# HOME must resolve ~/.ao for this user, or set AO_DATA_DIR / AO_RUN_FILE explicitly.
Environment=AO_HEADLESS=on
# Environment=AO_DATA_DIR=/var/lib/ao
# Environment=AO_RUN_FILE=/var/lib/ao/running.json
# Environment=AO_PORT=3001
ExecStart=/usr/local/bin/ao daemon
Restart=on-failure
RestartSec=2
TimeoutStopSec=20
KillSignal=SIGTERM

# Hardening knobs — keep write access to the AO state tree.
NoNewPrivileges=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

Notes:

- Prefer `Restart=on-failure` so an intentional `ao stop` / clean SIGTERM does
  not immediately bounce the unit; use `Restart=always` only if you want the
  supervisor to bring it back after every clean exit.
- `TimeoutStopSec` should be greater than `AO_SHUTDOWN_TIMEOUT`.
- Install the `ao` binary yourself (build from this repo, or copy from a release
  artifact). Do not treat npm as the install path for a headless host.
- After `systemctl start`, use `ao status` / `curl -s http://127.0.0.1:3001/readyz`
  from the same machine, then `ao lan enable` if you need the LAN listener.

## Local Electron on the same machine

The supervisor watchdog listens next to `running.json`. It arms after the first
local Electron client connects and can stop the daemon after the last client
disconnects. On a headless host that must keep running:

1. Start with `AO_HEADLESS=on` / `ao daemon --headless`, and
2. Expect that if a local Electron build connects and then quits, a
   **non-headless** daemon would still stop after the grace period.

Headless mode skips starting that watchdog so service lifetime stays with
systemd (or your process supervisor), not with a local window.