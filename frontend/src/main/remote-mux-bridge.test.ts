import { EventEmitter } from "node:events";
import { describe, expect, it, vi } from "vitest";

vi.mock("electron", () => ({
	ipcMain: { handle: vi.fn(), on: vi.fn() },
}));

import {
	createRemoteMuxBridge,
	MAX_MUX_CONNECTIONS_PER_WEBCONTENTS,
	REMOTE_MUX_FRAME_LIMIT,
	type RemoteMuxSender,
} from "./remote-mux-bridge";
import type { RemoteServerProfile } from "./remote-connection-store";

const profile: RemoteServerProfile = {
	id: "p1",
	label: "Lab",
	baseUrl: "http://100.64.0.1:3011",
	pinnedHostId: "h_lab",
	password: "secret12",
	createdAt: "2026-01-01T00:00:00.000Z",
	updatedAt: "2026-01-01T00:00:00.000Z",
};

class FakeSocket extends EventEmitter {
	static OPEN = 1;
	readyState = 0;
	headers: Record<string, string>;
	url: string;
	sent: string[] = [];
	closed = false;

	constructor(url: string, opts: { headers?: Record<string, string> }) {
		super();
		this.url = url;
		this.headers = opts.headers ?? {};
		queueMicrotask(() => {
			this.readyState = FakeSocket.OPEN;
			this.emit("open");
		});
	}

	send(data: string) {
		this.sent.push(data);
	}

	close() {
		if (this.closed) return;
		this.closed = true;
		this.readyState = 3;
		this.emit("close", 1000, Buffer.from("bye"));
	}
}

function mockSender(id = 1): RemoteMuxSender & { events: Array<{ channel: string; event: unknown }> } {
	const events: Array<{ channel: string; event: unknown }> = [];
	return {
		id,
		events,
		isDestroyed: () => false,
		send(channel, event) {
			events.push({ channel, event });
		},
		once() {
			return undefined;
		},
	};
}

describe("createRemoteMuxBridge", () => {
	it("gates identity then opens /mux with the bearer header", async () => {
		const fetchImpl = vi.fn(async () =>
			new Response(JSON.stringify({ hostId: "h_lab", apiVersion: 1 }), {
				status: 200,
				headers: { "content-type": "application/json" },
			}),
		);
		let constructed: FakeSocket | undefined;
		const bridge = createRemoteMuxBridge(() => "/tmp", {
			fetchImpl: fetchImpl as unknown as typeof fetch,
			getActiveProfile: async () => profile,
			webSocketImpl: class extends FakeSocket {
				constructor(url: string, opts: { headers?: Record<string, string> }) {
					super(url, opts);
					constructed = this;
				}
			} as unknown as typeof import("ws").WebSocket,
		});

		const sender = mockSender();
		const { connectionId } = await bridge.connect(sender);
		expect(connectionId).toBeTruthy();
		expect(fetchImpl).toHaveBeenCalledTimes(1);
		expect(fetchImpl.mock.calls.map((call) => String(call[0])).join("\n")).toContain(
			"/api/v1/identity",
		);
		expect(constructed?.url).toBe("ws://100.64.0.1:3011/mux");
		expect(constructed?.headers.Authorization).toBe("Bearer secret12");

		await vi.waitFor(() => {
			expect(sender.events.some((e) => (e.event as { type: string }).type === "open")).toBe(true);
		});

		bridge.send(sender, connectionId, JSON.stringify({ ch: "system", type: "ping" }));
		expect(constructed?.sent).toEqual([JSON.stringify({ ch: "system", type: "ping" })]);

		constructed?.emit("message", Buffer.from('{"ch":"system","type":"pong"}'), false);
		expect(
			sender.events.some(
				(e) =>
					(e.event as { type: string; data?: string }).type === "message" &&
					(e.event as { data?: string }).data === '{"ch":"system","type":"pong"}',
			),
		).toBe(true);
	});

	it("fails closed on host mismatch without opening a socket", async () => {
		const fetchImpl = vi.fn(async () =>
			new Response(JSON.stringify({ hostId: "h_other", apiVersion: 1 }), {
				status: 200,
				headers: { "content-type": "application/json" },
			}),
		);
		const ctor = vi.fn();
		const bridge = createRemoteMuxBridge(() => "/tmp", {
			fetchImpl: fetchImpl as unknown as typeof fetch,
			getActiveProfile: async () => profile,
			webSocketImpl: ctor as unknown as typeof import("ws").WebSocket,
		});
		await expect(bridge.connect(mockSender())).rejects.toThrow(/not sent|does not match/i);
		expect(ctor).not.toHaveBeenCalled();
	});

	it("drops oversized outbound frames", async () => {
		let constructed: FakeSocket | undefined;
		const bridge = createRemoteMuxBridge(() => "/tmp", {
			fetchImpl: vi.fn(async () =>
				new Response(JSON.stringify({ hostId: "h_lab", apiVersion: 1 }), { status: 200 }),
			) as unknown as typeof fetch,
			getActiveProfile: async () => profile,
			webSocketImpl: class extends FakeSocket {
				constructor(url: string, opts: { headers?: Record<string, string> }) {
					super(url, opts);
					constructed = this;
				}
			} as unknown as typeof import("ws").WebSocket,
		});
		const sender = mockSender();
		const { connectionId } = await bridge.connect(sender);
		await vi.waitFor(() => constructed?.readyState === FakeSocket.OPEN);
		bridge.send(sender, connectionId, "x".repeat(REMOTE_MUX_FRAME_LIMIT + 1));
		expect(constructed?.sent).toEqual([]);
	});

	it("caps concurrent mux connections per sender", async () => {
		const bridge = createRemoteMuxBridge(() => "/tmp", {
			fetchImpl: vi.fn(async () =>
				new Response(JSON.stringify({ hostId: "h_lab", apiVersion: 1 }), { status: 200 }),
			) as unknown as typeof fetch,
			getActiveProfile: async () => profile,
			webSocketImpl: FakeSocket as unknown as typeof import("ws").WebSocket,
		});
		const sender = mockSender();
		for (let i = 0; i < MAX_MUX_CONNECTIONS_PER_WEBCONTENTS; i += 1) {
			await bridge.connect(sender);
		}
		await expect(bridge.connect(sender)).rejects.toThrow(/already has/);
	});
});
