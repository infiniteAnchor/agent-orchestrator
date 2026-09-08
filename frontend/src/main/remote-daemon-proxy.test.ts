import { describe, expect, it, vi } from "vitest";

vi.mock("electron", () => ({
	ipcMain: {
		handle: vi.fn(),
		on: vi.fn(),
	},
}));

import {
	createRemoteDaemonProxy,
	MAX_STREAMS_PER_WEBCONTENTS,
	REMOTE_DAEMON_RESPONSE_BODY_LIMIT,
	REMOTE_DAEMON_STREAM_ERROR_MARKER,
	type RemoteDaemonStreamSender,
} from "./remote-daemon-proxy";
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

function jsonResponse(body: unknown, status = 200, headers?: Record<string, string>): Response {
	return new Response(typeof body === "string" ? body : JSON.stringify(body), {
		status,
		headers: { "content-type": "application/json", ...headers },
	});
}

function mockSender(id = 1): RemoteDaemonStreamSender & {
	events: Array<{ channel: string; event: unknown }>;
	destroy: () => void;
} {
	const events: Array<{ channel: string; event: unknown }> = [];
	let destroyed = false;
	const destroyListeners: Array<() => void> = [];
	return {
		id,
		events,
		isDestroyed: () => destroyed,
		send(channel, event) {
			events.push({ channel, event });
		},
		once(event, listener) {
			if (event === "destroyed") destroyListeners.push(listener);
			return undefined;
		},
		destroy() {
			if (destroyed) return;
			destroyed = true;
			for (const listener of destroyListeners.splice(0)) listener();
		},
	};
}

describe("createRemoteDaemonProxy", () => {
	it("verifies identity then attaches the bearer, discarding renderer Authorization", async () => {
		const calls: Array<{ url: string; authorization: string | null }> = [];
		const fetchImpl = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
			const headers = new Headers(init?.headers);
			calls.push({ url: String(input), authorization: headers.get("authorization") });
			if (String(input).endsWith("/api/v1/identity")) {
				return jsonResponse({ hostId: "h_lab", apiVersion: 1 });
			}
			return jsonResponse({ projects: [] });
		});

		const proxy = createRemoteDaemonProxy(() => "/tmp", {
			fetchImpl: fetchImpl as unknown as typeof fetch,
			getActiveProfile: async () => profile,
		});

		const result = await proxy.request({
			path: "/api/v1/projects",
			method: "GET",
			headers: { Authorization: "Bearer smuggled", Accept: "application/json" },
		});

		expect(result.status).toBe(200);
		expect(calls).toEqual([
			{ url: "http://100.64.0.1:3011/api/v1/identity", authorization: null },
			{ url: "http://100.64.0.1:3011/api/v1/projects", authorization: "Bearer secret12" },
		]);
		expect(JSON.parse(result.body)).toEqual({ projects: [] });
	});

	it("fails closed on host mismatch without sending the bearer", async () => {
		const fetchImpl = vi.fn(async (input: RequestInfo | URL) => {
			if (String(input).endsWith("/api/v1/identity")) {
				return jsonResponse({ hostId: "h_other", apiVersion: 1 });
			}
			throw new Error("authenticated fetch must not run");
		});

		const proxy = createRemoteDaemonProxy(() => "/tmp", {
			fetchImpl: fetchImpl as unknown as typeof fetch,
			getActiveProfile: async () => profile,
		});

		const result = await proxy.request({ path: "/api/v1/projects", method: "get" });
		expect(result.status).toBe(403);
		expect(JSON.parse(result.body).code).toBe("HOST_MISMATCH");
		expect(fetchImpl).toHaveBeenCalledTimes(1);
	});

	it("rejects loopback-only control paths before any network call", async () => {
		const fetchImpl = vi.fn();
		const proxy = createRemoteDaemonProxy(() => "/tmp", {
			fetchImpl: fetchImpl as unknown as typeof fetch,
			getActiveProfile: async () => profile,
		});

		await expect(proxy.request({ path: "/api/v1/desktop/sessions/x/workspace", method: "GET" })).rejects.toThrow(
			/not allowed/,
		);
		await expect(proxy.request({ path: "/api/v1/mobile/status", method: "GET" })).rejects.toThrow(/not allowed/);
		await expect(proxy.request({ path: "/shutdown", method: "POST" })).rejects.toThrow(/not allowed/);
		expect(fetchImpl).not.toHaveBeenCalled();
	});

	it("rejects path escape after URL normalization", async () => {
		const fetchImpl = vi.fn();
		const proxy = createRemoteDaemonProxy(() => "/tmp", {
			fetchImpl: fetchImpl as unknown as typeof fetch,
			getActiveProfile: async () => profile,
		});
		await expect(proxy.request({ path: "/api/v1/../shutdown", method: "POST" })).rejects.toThrow();
		expect(fetchImpl).not.toHaveBeenCalled();
	});

	it("returns 503 when no remote profile is active", async () => {
		const proxy = createRemoteDaemonProxy(() => "/tmp", {
			fetchImpl: vi.fn() as unknown as typeof fetch,
			getActiveProfile: async () => null,
		});
		const result = await proxy.request({ path: "/api/v1/projects", method: "GET" });
		expect(result.status).toBe(503);
		expect(JSON.parse(result.body).code).toBe("NO_REMOTE_PROFILE");
	});

	it("opens an SSE stream after identity succeeds and forwards chunks", async () => {
		const encoder = new TextEncoder();
		const fetchImpl = vi.fn(async (input: RequestInfo | URL) => {
			if (String(input).endsWith("/api/v1/identity")) {
				return jsonResponse({ hostId: "h_lab", apiVersion: 1 });
			}
			const stream = new ReadableStream<Uint8Array>({
				start(controller) {
					controller.enqueue(encoder.encode("data: one\n\n"));
					controller.enqueue(encoder.encode("data: two\n\n"));
					controller.close();
				},
			});
			return new Response(stream, { status: 200, headers: { "content-type": "text/event-stream" } });
		});

		const proxy = createRemoteDaemonProxy(() => "/tmp", {
			fetchImpl: fetchImpl as unknown as typeof fetch,
			getActiveProfile: async () => profile,
		});
		const sender = mockSender();
		const { streamId } = await proxy.openStream(sender, {
			path: "/api/v1/events",
			method: "GET",
		});
		expect(streamId).toBeTruthy();

		await vi.waitFor(() => {
			expect(sender.events.some((e) => (e.event as { type: string }).type === "end")).toBe(true);
		});
		const chunks = sender.events
			.filter((e) => (e.event as { type: string }).type === "chunk")
			.map((e) => (e.event as { data: string }).data)
			.join("");
		expect(chunks).toContain("data: one");
		expect(chunks).toContain("data: two");
	});

	it("caps concurrent streams per sender", async () => {
		const fetchImpl = vi.fn(async (input: RequestInfo | URL) => {
			if (String(input).endsWith("/api/v1/identity")) {
				return jsonResponse({ hostId: "h_lab", apiVersion: 1 });
			}
			// Never-ending body so streams stay open.
			const stream = new ReadableStream<Uint8Array>({
				start() {
					/* leave open */
				},
			});
			return new Response(stream, { status: 200 });
		});
		const proxy = createRemoteDaemonProxy(() => "/tmp", {
			fetchImpl: fetchImpl as unknown as typeof fetch,
			getActiveProfile: async () => profile,
		});
		const sender = mockSender();
		for (let i = 0; i < MAX_STREAMS_PER_WEBCONTENTS; i += 1) {
			await proxy.openStream(sender, { path: "/api/v1/events", method: "GET" });
		}
		await expect(proxy.openStream(sender, { path: "/api/v1/events", method: "GET" })).rejects.toThrow(
			new RegExp(`${REMOTE_DAEMON_STREAM_ERROR_MARKER} 429`),
		);
	});

	it("reserves stream capacity before awaiting authorization", async () => {
		let release!: () => void;
		const gate = new Promise<void>((resolve) => {
			release = resolve;
		});
		const fetchImpl = vi.fn(async (input: RequestInfo | URL) => {
			if (String(input).endsWith("/api/v1/identity")) {
				await gate;
				return jsonResponse({ hostId: "h_lab", apiVersion: 1 });
			}
			const stream = new ReadableStream<Uint8Array>({
				start() {
					/* leave open */
				},
			});
			return new Response(stream, { status: 200 });
		});
		const proxy = createRemoteDaemonProxy(() => "/tmp", {
			fetchImpl: fetchImpl as unknown as typeof fetch,
			getActiveProfile: async () => profile,
		});
		const sender = mockSender();
		const pending = Array.from({ length: MAX_STREAMS_PER_WEBCONTENTS + 1 }, () =>
			proxy.openStream(sender, { path: "/api/v1/events", method: "GET" }),
		);
		release();
		const results = await Promise.allSettled(pending);
		const fulfilled = results.filter((r) => r.status === "fulfilled");
		const rejected = results.filter((r) => r.status === "rejected");
		expect(fulfilled).toHaveLength(MAX_STREAMS_PER_WEBCONTENTS);
		expect(rejected).toHaveLength(1);
		expect(String((rejected[0] as PromiseRejectedResult).reason)).toMatch(
			new RegExp(`${REMOTE_DAEMON_STREAM_ERROR_MARKER} 429`),
		);
	});

	it("rejects oversized HTTP response bodies before IPC", async () => {
		const oversized = "x".repeat(REMOTE_DAEMON_RESPONSE_BODY_LIMIT + 1);
		const fetchImpl = vi.fn(async (input: RequestInfo | URL) => {
			if (String(input).endsWith("/api/v1/identity")) {
				return jsonResponse({ hostId: "h_lab", apiVersion: 1 });
			}
			return new Response(oversized, {
				status: 200,
				headers: { "content-type": "text/plain", "content-length": String(oversized.length) },
			});
		});
		const proxy = createRemoteDaemonProxy(() => "/tmp", {
			fetchImpl: fetchImpl as unknown as typeof fetch,
			getActiveProfile: async () => profile,
		});
		await expect(proxy.request({ path: "/api/v1/projects", method: "GET" })).rejects.toThrow(
			/IPC limit/i,
		);
	});

	it("bounds failed SSE error bodies instead of buffering forever", async () => {
		const oversized = "x".repeat((64 << 10) + 1);
		const fetchImpl = vi.fn(async (input: RequestInfo | URL) => {
			if (String(input).endsWith("/api/v1/identity")) {
				return jsonResponse({ hostId: "h_lab", apiVersion: 1 });
			}
			return new Response(oversized, {
				status: 500,
				headers: {
					"content-type": "application/json",
					"content-length": String(oversized.length),
				},
			});
		});
		const proxy = createRemoteDaemonProxy(() => "/tmp", {
			fetchImpl: fetchImpl as unknown as typeof fetch,
			getActiveProfile: async () => profile,
		});
		await expect(proxy.openStream(mockSender(), { path: "/api/v1/events", method: "GET" })).rejects.toThrow(
			new RegExp(`${REMOTE_DAEMON_STREAM_ERROR_MARKER} 500`),
		);
	});

	it("cancels pending SSE opens when the renderer is destroyed", async () => {
		let release!: () => void;
		const gate = new Promise<void>((resolve) => {
			release = resolve;
		});
		const fetchImpl = vi.fn(async (input: RequestInfo | URL) => {
			if (String(input).endsWith("/api/v1/identity")) {
				await gate;
				return jsonResponse({ hostId: "h_lab", apiVersion: 1 });
			}
			throw new Error("authenticated fetch must not run after cancel");
		});
		const proxy = createRemoteDaemonProxy(() => "/tmp", {
			fetchImpl: fetchImpl as unknown as typeof fetch,
			getActiveProfile: async () => profile,
		});
		const sender = mockSender();
		const pending = proxy.openStream(sender, { path: "/api/v1/events", method: "GET" });
		sender.destroy();
		release();
		await expect(pending).rejects.toThrow(
			new RegExp(`${REMOTE_DAEMON_STREAM_ERROR_MARKER} 499`),
		);
	});
});
