# Native task execution acceptance

Captured from the real Linux Electron checkout on 2026-10-02 using isolated AO
state, real daemons, the real preload bridge, and installed Codex workers.
No mock responses or browser-only bridge were used.

- [Local task-plan browsing](local.png).
- [Completed remote plan](remote.png).
- [Completed remote plan in a narrow window](remote-narrow.png).

The remote view used the daemon's authenticated LAN listener on the same host.
See the [acceptance record](../../plans/task-execution-acceptance.md) for execution,
validation, cleanup, and the detached Chat recovery limitation.
