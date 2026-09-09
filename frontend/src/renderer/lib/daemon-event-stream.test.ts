import { beforeEach, describe, expect, it, vi } from "vitest";

const { getApiBaseUrlMock } = vi.hoisted(() => ({
	getApiBaseUrlMock: vi.fn(() => "http://127.0.0.1:3001"),
}));

vi.mock("./api-client", () => ({ getApiBaseUrl: getApiBaseUrlMock }));

const { remoteDaemon } = vi.hoisted(() => ({
	remoteDaemon: {
		openStream: vi.fn(),
		closeStream: vi.fn(),
		onStreamEvent: vi.fn(),
	},
}));

vi.mock("./bridge", () => ({ aoBridge: { remoteDaemon } }));

import { setDaemonConnection } from "./daemon-connection";
import {
	createDaemonEventStream,
	DAEMON_EVENT_SOURCE_CLOSED,
	DAEMON_EVENT_SOURCE_OPEN,
	resetRemoteEventCursors,
} from "./daemon-event-stream";

type StreamEvent = { type: "chunk"; data: string } | { type: "end" } | { type: "error"; message: string };

function flush(): Promise<void> {
	return new Promise((resolve) => setTimeout(resolve, 0));
}

describe("daemon event stream (remote)", () => {
	let listeners: Array<(event: StreamEvent) => void>;
	let nextStreamId: number;

	beforeEach(() => {
		resetRemoteEventCursors();
		setDaemonConnection({ kind: "local" }, null);
		listeners = [];
		nextStreamId = 1;
		remoteDaemon.openStream.mockReset();
		remoteDaemon.closeStream.mockReset();
		remoteDaemon.onStreamEvent.mockReset();
		remoteDaemon.openStream.mockImplementation(async () => ({ streamId: `s${nextStreamId++}` }));
		remoteDaemon.onStreamEvent.mockImplementation(
			(_streamId: string, listener: (event: StreamEvent) => void) => {
				listeners.push(listener);
				return () => {
					listeners = listeners.filter((item) => item !== listener);
				};
			},
		);
		setDaemonConnection({ kind: "remote", profileId: "profile-1" }, "http://100.64.0.5:4317");
	});

	it("opens through the bridge, dispatches named frames, and reports open", async () => {
		const stream = createDaemonEventStream("/api/v1/events");
		const onOpen = vi.fn();
		const onEvent = vi.fn();
		stream.onopen = onOpen;
		stream.addEventListener("session_created", onEvent);
		await flush();

		expect(remoteDaemon.openStream).toHaveBeenCalledWith({
			path: "/api/v1/events",
			method: "GET",
			headers: { Accept: "text/event-stream" },
		});
		expect(stream.readyState).toBe(DAEMON_EVENT_SOURCE_OPEN);
		expect(onOpen).toHaveBeenCalledTimes(1);

		listeners[0]({ type: "chunk", data: 'id: 7\nevent: session_created\ndata: {"sessionId":"ao-1"}\n\n' });
		expect(onEvent).toHaveBeenCalledTimes(1);
		expect((onEvent.mock.calls[0][0] as MessageEvent).data).toBe('{"sessionId":"ao-1"}');
	});

	it("resumes from the last event id after the stream ends", async () => {
		const first = createDaemonEventStream("/api/v1/events");
		await flush();
		const onError = vi.fn();
		first.onerror = onError;
		listeners[0]({ type: "chunk", data: "id: 42\nevent: session_updated\ndata: {}\n\n" });
		listeners[0]({ type: "end" });

		expect(onError).toHaveBeenCalledTimes(1);
		expect(first.readyState).toBe(DAEMON_EVENT_SOURCE_CLOSED);

		createDaemonEventStream("/api/v1/events");
		await flush();
		expect(remoteDaemon.openStream).toHaveBeenLastCalledWith(
			expect.objectContaining({ path: "/api/v1/events?after=42" }),
		);
	});

	it("keeps cursors per server so switching servers does not reuse a sequence", async () => {
		createDaemonEventStream("/api/v1/events");
		await flush();
		listeners[0]({ type: "chunk", data: "id: 99\nevent: session_updated\ndata: {}\n\n" });
		listeners[0]({ type: "end" });

		setDaemonConnection({ kind: "remote", profileId: "profile-2" }, "http://100.64.0.6:4317");
		createDaemonEventStream("/api/v1/events");
		await flush();
		expect(remoteDaemon.openStream).toHaveBeenLastCalledWith(
			expect.objectContaining({ path: "/api/v1/events" }),
		);
	});

	it("closes the proxied stream when the caller closes", async () => {
		const stream = createDaemonEventStream("/api/v1/events");
		await flush();
		stream.close();
		expect(remoteDaemon.closeStream).toHaveBeenCalledWith("s1");
		expect(stream.readyState).toBe(DAEMON_EVENT_SOURCE_CLOSED);
	});

	it("fails closed when the stream cannot be opened", async () => {
		remoteDaemon.openStream.mockRejectedValue(new Error("no remote AO server is selected"));
		const stream = createDaemonEventStream("/api/v1/events");
		const onError = vi.fn();
		stream.onerror = onError;
		await flush();
		expect(onError).toHaveBeenCalledTimes(1);
		expect(stream.readyState).toBe(DAEMON_EVENT_SOURCE_CLOSED);
	});
});

describe("daemon event stream (local)", () => {
	beforeEach(() => {
		resetRemoteEventCursors();
		setDaemonConnection({ kind: "local" }, null);
		getApiBaseUrlMock.mockReturnValue("http://127.0.0.1:3001");
	});

	it("uses a browser EventSource on loopback", () => {
		const instances: Array<{ url: string }> = [];
		class EventSourceStub {
			readyState = 0;
			onopen: (() => void) | null = null;
			onerror: (() => void) | null = null;
			onmessage: ((event: MessageEvent) => void) | null = null;
			constructor(public url: string) {
				instances.push(this);
			}
			addEventListener() {}
			close() {}
		}
		vi.stubGlobal("EventSource", EventSourceStub);
		try {
			createDaemonEventStream("/api/v1/events");
			expect(instances).toHaveLength(1);
			expect(instances[0].url).toBe("http://127.0.0.1:3001/api/v1/events");
		} finally {
			vi.unstubAllGlobals();
		}
	});
});
