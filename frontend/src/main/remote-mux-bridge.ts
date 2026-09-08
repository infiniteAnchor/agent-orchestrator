// Main-process WebSocket client for remote AO /mux (Phase 2).
//
// Browser WebSocket cannot set Authorization. Main opens the socket with the
// enrolled LAN bearer after the pinned host-id identity gate, then relays
// bounded text frames over IPC.

import { ipcMain } from "electron";
import { randomUUID } from "node:crypto";
import { WebSocket } from "ws";
import {
	getActiveRemoteProfile,
	type RemoteServerProfile,
} from "./remote-connection-store";
import {
	verifyPinnedIdentity,
	type IdentityGateResult,
} from "./remote-identity-gate";

export const REMOTE_MUX_CONNECT_CHANNEL = "remoteMux:connect";
export const REMOTE_MUX_SEND_CHANNEL = "remoteMux:send";
export const REMOTE_MUX_CLOSE_CHANNEL = "remoteMux:close";

export function remoteMuxEventChannel(connectionId: string): string {
	return `remoteMux:event:${connectionId}`;
}

/** Match backend terminalMuxReadLimit (1 MiB). */
export const REMOTE_MUX_FRAME_LIMIT = 1 << 20;

export const MAX_MUX_CONNECTIONS_PER_WEBCONTENTS = 4;

export type RemoteMuxClientEvent =
	| { type: "open" }
	| { type: "message"; data: string }
	| { type: "close"; code?: number; reason?: string }
	| { type: "error"; message: string };

export interface RemoteMuxSender {
	readonly id: number;
	isDestroyed(): boolean;
	send(channel: string, event: RemoteMuxClientEvent): void;
	once(event: "destroyed", listener: () => void): unknown;
}

export interface RemoteMuxBridge {
	connect(sender: RemoteMuxSender): Promise<{ connectionId: string }>;
	send(sender: Pick<RemoteMuxSender, "id">, connectionId: unknown, data: unknown): void;
	close(sender: Pick<RemoteMuxSender, "id">, connectionId: unknown): void;
}

export interface RemoteMuxBridgeOptions {
	getActiveProfile?: () => Promise<RemoteServerProfile | null>;
	verifyIdentity?: (options: {
		baseUrl: string;
		pinnedHostId: string;
		fetchImpl?: typeof fetch;
	}) => Promise<IdentityGateResult>;
	fetchImpl?: typeof fetch;
	/** Test seam for the WebSocket constructor. */
	webSocketImpl?: typeof WebSocket;
}

function muxWsUrl(baseUrl: string): string {
	const url = new URL(baseUrl);
	url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
	url.pathname = `${url.pathname.replace(/\/+$/, "")}/mux`;
	url.search = "";
	url.hash = "";
	return url.toString();
}

interface ActiveMuxConnection {
	socket: WebSocket;
	sender: RemoteMuxSender;
	channel: string;
	closed: boolean;
}

export function createRemoteMuxBridge(
	getStateDir: () => string,
	options: RemoteMuxBridgeOptions = {},
): RemoteMuxBridge {
	const getActiveProfile =
		options.getActiveProfile ?? (() => getActiveRemoteProfile(getStateDir()));
	const verifyIdentity = options.verifyIdentity ?? verifyPinnedIdentity;
	const doFetch = options.fetchImpl;
	const WebSocketImpl = options.webSocketImpl ?? WebSocket;

	const connections = new Map<string, ActiveMuxConnection>();
	const idsBySender = new Map<number, Set<string>>();

	function teardown(connectionId: string, conn: ActiveMuxConnection, emitClose = true): void {
		if (conn.closed) return;
		conn.closed = true;
		connections.delete(connectionId);
		idsBySender.get(conn.sender.id)?.delete(connectionId);
		try {
			conn.socket.close();
		} catch {
			// already closing
		}
		if (emitClose && !conn.sender.isDestroyed()) {
			conn.sender.send(conn.channel, { type: "close" });
		}
	}

	function trackSender(sender: RemoteMuxSender): Set<string> {
		let ids = idsBySender.get(sender.id);
		if (ids === undefined) {
			ids = new Set();
			idsBySender.set(sender.id, ids);
			sender.once("destroyed", () => {
				const orphaned = idsBySender.get(sender.id);
				idsBySender.delete(sender.id);
				for (const connectionId of orphaned ?? []) {
					const conn = connections.get(connectionId);
					if (conn !== undefined) teardown(connectionId, conn, false);
				}
			});
		}
		return ids;
	}

	async function connect(sender: RemoteMuxSender): Promise<{ connectionId: string }> {
		const ids = trackSender(sender);
		if (ids.size >= MAX_MUX_CONNECTIONS_PER_WEBCONTENTS) {
			throw new Error(
				`This window already has ${MAX_MUX_CONNECTIONS_PER_WEBCONTENTS} open remote mux connections.`,
			);
		}
		// Reserve capacity before any await so concurrent connect calls cannot all
		// pass the cap while profile lookup / identity verification are in flight.
		const connectionId = randomUUID();
		ids.add(connectionId);
		const releaseReservation = (): void => {
			ids.delete(connectionId);
		};

		try {
			const profile = await getActiveProfile();
			if (profile === null) {
				throw new Error("No remote AO server is selected.");
			}
			const gate = await verifyIdentity({
				baseUrl: profile.baseUrl,
				pinnedHostId: profile.pinnedHostId,
				fetchImpl: doFetch,
			});
			if (!gate.ok) {
				throw new Error(gate.message);
			}

			const channel = remoteMuxEventChannel(connectionId);
			const socket = new WebSocketImpl(muxWsUrl(profile.baseUrl), {
				headers: { Authorization: `Bearer ${profile.password}` },
				maxPayload: REMOTE_MUX_FRAME_LIMIT,
			});

			const conn: ActiveMuxConnection = {
				socket,
				sender,
				channel,
				closed: false,
			};
			connections.set(connectionId, conn);

			socket.on("open", () => {
				if (conn.closed || sender.isDestroyed()) return;
				sender.send(channel, { type: "open" });
			});
			socket.on("message", (data, isBinary) => {
				if (conn.closed || sender.isDestroyed()) return;
				if (isBinary) {
					sender.send(channel, { type: "error", message: "Binary mux frames are not supported." });
					return;
				}
				const text = typeof data === "string" ? data : data.toString("utf8");
				if (Buffer.byteLength(text, "utf8") > REMOTE_MUX_FRAME_LIMIT) {
					sender.send(channel, { type: "error", message: "Mux frame exceeds size limit." });
					teardown(connectionId, conn);
					return;
				}
				sender.send(channel, { type: "message", data: text });
			});
			socket.on("close", (code, reason) => {
				if (conn.closed) return;
				conn.closed = true;
				connections.delete(connectionId);
				idsBySender.get(sender.id)?.delete(connectionId);
				if (!sender.isDestroyed()) {
					sender.send(channel, {
						type: "close",
						code,
						reason: reason.toString("utf8"),
					});
				}
			});
			socket.on("error", (err) => {
				if (conn.closed || sender.isDestroyed()) return;
				sender.send(channel, {
					type: "error",
					message: err instanceof Error ? err.message : String(err),
				});
			});

			return { connectionId };
		} catch (error) {
			releaseReservation();
			throw error;
		}
	}

	function send(sender: Pick<RemoteMuxSender, "id">, connectionId: unknown, data: unknown): void {
		if (typeof connectionId !== "string" || typeof data !== "string") return;
		if (Buffer.byteLength(data, "utf8") > REMOTE_MUX_FRAME_LIMIT) return;
		const conn = connections.get(connectionId);
		if (conn === undefined || conn.sender.id !== sender.id || conn.closed) return;
		if (conn.socket.readyState !== WebSocketImpl.OPEN) return;
		conn.socket.send(data);
	}

	function close(sender: Pick<RemoteMuxSender, "id">, connectionId: unknown): void {
		if (typeof connectionId !== "string") return;
		const conn = connections.get(connectionId);
		if (conn === undefined || conn.sender.id !== sender.id) return;
		teardown(connectionId, conn);
	}

	return { connect, send, close };
}

export function installRemoteMuxBridge(getStateDir: () => string): void {
	const bridge = createRemoteMuxBridge(getStateDir);
	ipcMain.handle(REMOTE_MUX_CONNECT_CHANNEL, (event) => bridge.connect(event.sender));
	ipcMain.on(REMOTE_MUX_SEND_CHANNEL, (event, connectionId: unknown, data: unknown) => {
		bridge.send(event.sender, connectionId, data);
	});
	ipcMain.on(REMOTE_MUX_CLOSE_CHANNEL, (event, connectionId: unknown) => {
		bridge.close(event.sender, connectionId);
	});
}
