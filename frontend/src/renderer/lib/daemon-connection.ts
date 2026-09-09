// Which daemon the renderer is talking to: the local one over loopback, or an
// enrolled remote AO server reached through the Electron main process.
//
// The connection secret never reaches the renderer. In remote mode the base URL
// here is public identity/display information only; every REST call, event
// stream, and terminal socket goes through the main-process bridge, which owns
// the bearer and the pinned host-id gate.

import type { ConnectionMode } from "../../shared/remote-connection";

let mode: ConnectionMode = { kind: "local" };
let remoteBaseUrl: string | null = null;
const listeners = new Set<() => void>();

export function getDaemonConnectionMode(): ConnectionMode {
	return mode;
}

export function isRemoteDaemon(): boolean {
	return mode.kind === "remote";
}

/** Public base URL of the active remote server, or null in local mode. */
export function getRemoteDaemonBaseUrl(): string | null {
	return mode.kind === "remote" ? remoteBaseUrl : null;
}

/**
 * Subscribe to connection changes (useSyncExternalStore-compatible). Long-lived
 * transports use this to rebind when the user switches servers.
 */
export function subscribeDaemonConnection(listener: () => void): () => void {
	listeners.add(listener);
	return () => {
		listeners.delete(listener);
	};
}

/**
 * Record the active connection. Called from the daemon-status handshake so the
 * renderer learns about a switch before any request is issued.
 */
export function setDaemonConnection(next: ConnectionMode, baseUrl: string | null): void {
	const nextBaseUrl = next.kind === "remote" ? baseUrl : null;
	const unchanged =
		next.kind === mode.kind &&
		nextBaseUrl === remoteBaseUrl &&
		(next.kind !== "remote" || (mode.kind === "remote" && next.profileId === mode.profileId));
	if (unchanged) return;
	mode = next;
	remoteBaseUrl = nextBaseUrl;
	listeners.forEach((listener) => listener());
}
