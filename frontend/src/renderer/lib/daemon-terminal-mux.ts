// Terminal mux factory that picks the transport for the active connection.
//
// Local mode opens a browser WebSocket straight to the loopback daemon. Remote
// mode uses the IPC-backed WebSocket stand-in, because a browser WebSocket cannot
// attach the LAN bearer and main owns the authenticated upgrade.

import { isRemoteDaemon } from "./daemon-connection";
import { createRemoteMuxWebSocketClass } from "./remote-mux-socket";
import { createTerminalMux, type TerminalMux } from "./terminal-mux";

/**
 * Create a mux client for the active connection. `url` is the loopback-derived
 * ws URL; the remote implementation ignores it and connects through main.
 */
export function createDaemonTerminalMux(url: string): TerminalMux {
	if (isRemoteDaemon()) {
		return createTerminalMux(url, createRemoteMuxWebSocketClass());
	}
	return createTerminalMux(url);
}
