// One SSE abstraction for both connection modes.
//
// Local mode returns a thin wrapper over the browser EventSource, preserving its
// auto-reconnect and Last-Event-ID behavior. Remote mode has no browser
// transport (EventSource cannot attach the LAN bearer and main owns the socket),
// so it opens the stream through the main-process proxy and feeds decoded chunks
// through the shared SSE frame parser. Because the proxied stream does not
// auto-reconnect, the remote adapter remembers the last `id:` per
// (server, path) and resumes with `?after=` on the next open, which is the
// daemon's durable CDC cursor.

import type { RemoteDaemonStreamEvent } from "../../main/remote-daemon-proxy";
import { aoBridge } from "./bridge";
import { createSseFrameParser } from "./cloud-cp/sse";
import { getApiBaseUrl } from "./api-client";
import { getDaemonConnectionMode, isRemoteDaemon } from "./daemon-connection";

export const DAEMON_EVENT_SOURCE_CONNECTING = 0;
export const DAEMON_EVENT_SOURCE_OPEN = 1;
export const DAEMON_EVENT_SOURCE_CLOSED = 2;

export type DaemonEventStreamListener = (event: MessageEvent) => void;

export interface DaemonEventStream {
	readonly readyState: number;
	onopen: (() => void) | null;
	onerror: (() => void) | null;
	onmessage: ((event: MessageEvent) => void) | null;
	addEventListener(type: string, listener: DaemonEventStreamListener): void;
	close(): void;
}

/** Last event id seen per remote (profile, path), used to resume after a drop. */
const remoteCursors = new Map<string, string>();

function cursorKey(path: string): string {
	const mode = getDaemonConnectionMode();
	return mode.kind === "remote" ? `${mode.profileId}|${path}` : `local|${path}`;
}

/** Test seam: forget every remembered remote cursor. */
export function resetRemoteEventCursors(): void {
	remoteCursors.clear();
}

class LocalDaemonEventStream implements DaemonEventStream {
	private readonly source: EventSource;
	private openHandler: (() => void) | null = null;
	private errorHandler: (() => void) | null = null;
	private messageHandler: ((event: MessageEvent) => void) | null = null;

	constructor(path: string) {
		const baseUrl = getApiBaseUrl().replace(/\/+$/, "");
		this.source = new EventSource(`${baseUrl}${path}`);
		this.source.onopen = () => this.openHandler?.();
		this.source.onerror = () => this.errorHandler?.();
		this.source.onmessage = (event) => this.messageHandler?.(event);
	}

	get readyState(): number {
		return this.source.readyState;
	}

	get onopen(): (() => void) | null {
		return this.openHandler;
	}

	set onopen(handler: (() => void) | null) {
		this.openHandler = handler;
	}

	get onerror(): (() => void) | null {
		return this.errorHandler;
	}

	set onerror(handler: (() => void) | null) {
		this.errorHandler = handler;
	}

	get onmessage(): ((event: MessageEvent) => void) | null {
		return this.messageHandler;
	}

	set onmessage(handler: ((event: MessageEvent) => void) | null) {
		this.messageHandler = handler;
	}

	addEventListener(type: string, listener: DaemonEventStreamListener): void {
		this.source.addEventListener(type, listener as EventListener);
	}

	close(): void {
		this.source.close();
	}
}

class ClosedDaemonEventStream implements DaemonEventStream {
	readonly readyState = DAEMON_EVENT_SOURCE_CLOSED;
	onopen: (() => void) | null = null;
	onerror: (() => void) | null = null;
	onmessage: ((event: MessageEvent) => void) | null = null;
	addEventListener(): void {
		// A stream that never opened has no events to deliver.
	}
	close(): void {
		// Nothing to close.
	}
}

class RemoteDaemonEventStream implements DaemonEventStream {
	readyState = DAEMON_EVENT_SOURCE_CONNECTING;
	onopen: (() => void) | null = null;
	onerror: (() => void) | null = null;
	onmessage: ((event: MessageEvent) => void) | null = null;

	private readonly key: string;
	private readonly listeners = new Map<string, Set<DaemonEventStreamListener>>();
	private readonly parser = createSseFrameParser();
	private unsubscribe: (() => void) | null = null;
	private streamId: string | null = null;
	private closed = false;

	constructor(path: string) {
		this.key = cursorKey(path);
		void this.start(path);
	}

	private async start(path: string): Promise<void> {
		const after = remoteCursors.get(this.key);
		const requestPath =
			after === undefined
				? path
				: `${path}${path.includes("?") ? "&" : "?"}after=${encodeURIComponent(after)}`;
		let streamId: string;
		try {
			({ streamId } = await aoBridge.remoteDaemon.openStream({
				path: requestPath,
				method: "GET",
				headers: { Accept: "text/event-stream" },
			}));
		} catch {
			this.fail();
			return;
		}
		if (this.closed) {
			// Closed while opening: main already holds the stream, so release it.
			aoBridge.remoteDaemon.closeStream(streamId);
			return;
		}
		this.streamId = streamId;
		// Subscribe in the same microtask as the open resolution: main defers the
		// first chunk, so this listener is attached before any event lands.
		this.unsubscribe = aoBridge.remoteDaemon.onStreamEvent(streamId, (event) =>
			this.onStreamEvent(event),
		);
		this.readyState = DAEMON_EVENT_SOURCE_OPEN;
		this.onopen?.();
	}

	private onStreamEvent(event: RemoteDaemonStreamEvent): void {
		if (this.closed) return;
		if (event.type === "chunk") {
			for (const frame of this.parser.push(event.data)) this.deliver(frame);
			return;
		}
		if (event.type === "end") {
			for (const frame of this.parser.flush()) this.deliver(frame);
			this.fail();
			return;
		}
		this.fail();
	}

	private deliver(frame: { id?: string; event?: string; data: string }): void {
		if (frame.id !== undefined) remoteCursors.set(this.key, frame.id);
		const type = frame.event ?? "message";
		const message = new MessageEvent(type, { data: frame.data, lastEventId: frame.id ?? "" });
		if (type === "message") this.onmessage?.(message);
		const listeners = this.listeners.get(type);
		if (listeners === undefined) return;
		for (const listener of listeners) listener(message);
	}

	private fail(): void {
		if (this.closed) return;
		this.closed = true;
		this.readyState = DAEMON_EVENT_SOURCE_CLOSED;
		this.unsubscribe?.();
		this.unsubscribe = null;
		this.onerror?.();
	}

	addEventListener(type: string, listener: DaemonEventStreamListener): void {
		const set = this.listeners.get(type) ?? new Set<DaemonEventStreamListener>();
		set.add(listener);
		this.listeners.set(type, set);
	}

	close(): void {
		if (this.closed) return;
		this.closed = true;
		this.readyState = DAEMON_EVENT_SOURCE_CLOSED;
		this.unsubscribe?.();
		this.unsubscribe = null;
		if (this.streamId !== null) aoBridge.remoteDaemon.closeStream(this.streamId);
		this.streamId = null;
	}
}

/**
 * Open a daemon event stream for a daemon-relative path (e.g. "/api/v1/events").
 * The caller owns `close()`; a dropped remote stream surfaces through `onerror`
 * with `readyState === CLOSED`, matching EventSource's terminal state.
 */
export function createDaemonEventStream(path: string): DaemonEventStream {
	if (isRemoteDaemon()) return new RemoteDaemonEventStream(path);
	if (typeof EventSource === "undefined" || getApiBaseUrl() === "") {
		return new ClosedDaemonEventStream();
	}
	// A construction failure propagates, matching the raw EventSource contract
	// callers rely on to schedule their own retry/backoff.
	return new LocalDaemonEventStream(path);
}
