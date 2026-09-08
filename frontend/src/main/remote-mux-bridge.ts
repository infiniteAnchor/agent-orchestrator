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
export const REMOTE_MUX_SUBSCRIBE_CHANNEL = "remoteMux:subscribe";
export const REMOTE_MUX_SEND_CHANNEL = "remoteMux:send";
export const REMOTE_MUX_CLOSE_CHANNEL = "remoteMux:close";

export function remoteMuxEventChannel(connectionId: string): string {
	return `remoteMux:event:${connectionId}`;
}

/** Match backend terminalMuxReadLimit (1 MiB). */
export const REMOTE_MUX_FRAME_LIMIT = 1 << 20;

export const MAX_MUX_CONNECTIONS_PER_WEBCONTENTS = 4;

/** Bound deferred events until the renderer subscribes to the IPC channel. */
export const MAX_PENDING_MUX_EVENTS = 64;

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
	subscribe(sender: Pick<RemoteMuxSender, "id">, connectionId: unknown): void;
	send(sender: Pick<RemoteMuxSender, "id">, connectionId: unknown, data: unknown): void;
	close(sender: Pick<RemoteMuxSender, "id">, connectionId: unknown): void;
}

export interface RemoteMuxBridgeOptions {
	getActiveProfile?: () => Promise<RemoteServerProfile | null>;
	verifyIdentity?: (options: {
		baseUrl: string;
		pinnedHostId: string;
		fetchImpl?: typeof fetch;
		signal?: AbortSignal;
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
	connectionId: string;
	socket: WebSocket;
	sender: RemoteMuxSender;
	channel: string;
	closed: boolean;
	subscribed: boolean;
	pendingEvents: RemoteMuxClientEvent[];
}

interface PendingMuxConnect {
	cancelled: boolean;
	abort: AbortController;
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
	const pendingConnects = new Map<string, PendingMuxConnect>();
	const idsBySender = new Map<number, Set<string>>();

	function deliverEvent(conn: ActiveMuxConnection, event: RemoteMuxClientEvent): void {
		if (conn.sender.isDestroyed()) return;
		if (!conn.subscribed) {
			if (conn.pendingEvents.length >= MAX_PENDING_MUX_EVENTS && event.type !== "close") {
				// Drop buffered payloads but keep a terminal close tombstone so
				// subscribe can still move the renderer out of CONNECTING.
				conn.pendingEvents.length = 0;
				conn.pendingEvents.push({
					type: "close",
					reason: "Mux event buffer overflow before subscribe.",
				});
				if (!conn.closed) {
					conn.closed = true;
					pendingConnects.delete(conn.connectionId);
					try {
						conn.socket.close();
					} catch {
						// already closing
					}
				}
				return;
			}
			if (event.type === "close") {
				// Prefer the real close over any overflow placeholder / payloads.
				conn.pendingEvents = [event];
			} else {
				conn.pendingEvents.push(event);
			}
			return;
		}
		conn.sender.send(conn.channel, event);
	}

	function emitEvent(conn: ActiveMuxConnection, event: RemoteMuxClientEvent): void {
		if (conn.closed) return;
		deliverEvent(conn, event);
	}

	/** Drop a connection immediately without replaying buffered events. */
	function discardConnection(connectionId: string, conn: ActiveMuxConnection): void {
		conn.closed = true;
		connections.delete(connectionId);
		pendingConnects.delete(connectionId);
		idsBySender.get(conn.sender.id)?.delete(connectionId);
		try {
			conn.socket.close();
		} catch {
			// already closing
		}
	}

	/**
	 * Mark the socket closed. If the renderer has not subscribed yet, keep a
	 * tombstone so buffered terminal events (especially close) can still be
	 * replayed when subscribe arrives.
	 */
	function markClosed(
		connectionId: string,
		conn: ActiveMuxConnection,
		closeEvent: Extract<RemoteMuxClientEvent, { type: "close" }> = { type: "close" },
	): void {
		if (conn.closed) return;
		conn.closed = true;
		pendingConnects.delete(connectionId);
		try {
			conn.socket.close();
		} catch {
			// already closing
		}
		deliverEvent(conn, closeEvent);
		if (conn.subscribed) {
			connections.delete(connectionId);
			idsBySender.get(conn.sender.id)?.delete(connectionId);
		}
	}

	function teardown(connectionId: string, conn: ActiveMuxConnection, emitClose = true): void {
		if (conn.closed && !connections.has(connectionId)) return;
		if (emitClose) {
			markClosed(connectionId, conn);
			if (!conn.subscribed) {
				// Explicit local close: no need to wait for subscribe.
				discardConnection(connectionId, conn);
			}
			return;
		}
		discardConnection(connectionId, conn);
	}

	function cancelPendingConnect(connectionId: string): void {
		const pending = pendingConnects.get(connectionId);
		if (pending === undefined) return;
		pending.cancelled = true;
		pending.abort.abort();
		pendingConnects.delete(connectionId);
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
					cancelPendingConnect(connectionId);
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
		const pending: PendingMuxConnect = { cancelled: false, abort: new AbortController() };
		pendingConnects.set(connectionId, pending);
		const releaseReservation = (): void => {
			cancelPendingConnect(connectionId);
			ids.delete(connectionId);
		};

		try {
			const profile = await getActiveProfile();
			if (pending.cancelled || sender.isDestroyed()) {
				throw new Error("Remote mux connect cancelled.");
			}
			if (profile === null) {
				throw new Error("No remote AO server is selected.");
			}
			const gate = await verifyIdentity({
				baseUrl: profile.baseUrl,
				pinnedHostId: profile.pinnedHostId,
				fetchImpl: doFetch,
				signal: pending.abort.signal,
			});
			if (pending.cancelled || sender.isDestroyed()) {
				throw new Error("Remote mux connect cancelled.");
			}
			if (!gate.ok) {
				throw new Error(gate.message);
			}

			const channel = remoteMuxEventChannel(connectionId);
			const socket = new WebSocketImpl(muxWsUrl(profile.baseUrl), {
				headers: { Authorization: `Bearer ${profile.password}` },
				maxPayload: REMOTE_MUX_FRAME_LIMIT,
			});

			pendingConnects.delete(connectionId);
			const conn: ActiveMuxConnection = {
				connectionId,
				socket,
				sender,
				channel,
				closed: false,
				subscribed: false,
				pendingEvents: [],
			};
			connections.set(connectionId, conn);

			socket.on("open", () => {
				emitEvent(conn, { type: "open" });
			});
			socket.on("message", (data, isBinary) => {
				if (conn.closed || sender.isDestroyed()) return;
				if (isBinary) {
					emitEvent(conn, { type: "error", message: "Binary mux frames are not supported." });
					return;
				}
				const text = typeof data === "string" ? data : data.toString("utf8");
				if (Buffer.byteLength(text, "utf8") > REMOTE_MUX_FRAME_LIMIT) {
					emitEvent(conn, { type: "error", message: "Mux frame exceeds size limit." });
					teardown(connectionId, conn);
					return;
				}
				emitEvent(conn, { type: "message", data: text });
			});
			socket.on("close", (code, reason) => {
				markClosed(connectionId, conn, {
					type: "close",
					code,
					reason: reason.toString("utf8"),
				});
			});
			socket.on("error", (err) => {
				emitEvent(conn, {
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

	function subscribe(sender: Pick<RemoteMuxSender, "id">, connectionId: unknown): void {
		if (typeof connectionId !== "string") return;
		const conn = connections.get(connectionId);
		if (conn === undefined || conn.sender.id !== sender.id) return;
		if (conn.subscribed) return;
		conn.subscribed = true;
		const queued = conn.pendingEvents.splice(0);
		for (const event of queued) {
			if (conn.sender.isDestroyed()) return;
			conn.sender.send(conn.channel, event);
		}
		// Closed tombstones are retained only until subscribe replays terminal state.
		if (conn.closed) {
			connections.delete(connectionId);
			idsBySender.get(sender.id)?.delete(connectionId);
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
		cancelPendingConnect(connectionId);
		const conn = connections.get(connectionId);
		if (conn === undefined || conn.sender.id !== sender.id) return;
		teardown(connectionId, conn);
	}

	return { connect, subscribe, send, close };
}

export function installRemoteMuxBridge(getStateDir: () => string): void {
	const bridge = createRemoteMuxBridge(getStateDir);
	ipcMain.handle(REMOTE_MUX_CONNECT_CHANNEL, (event) => bridge.connect(event.sender));
	ipcMain.on(REMOTE_MUX_SUBSCRIBE_CHANNEL, (event, connectionId: unknown) => {
		bridge.subscribe(event.sender, connectionId);
	});
	ipcMain.on(REMOTE_MUX_SEND_CHANNEL, (event, connectionId: unknown, data: unknown) => {
		bridge.send(event.sender, connectionId, data);
	});
	ipcMain.on(REMOTE_MUX_CLOSE_CHANNEL, (event, connectionId: unknown) => {
		bridge.close(event.sender, connectionId);
	});
}
