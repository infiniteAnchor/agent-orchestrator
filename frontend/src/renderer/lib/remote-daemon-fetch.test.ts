import { beforeEach, describe, expect, it, vi } from "vitest";

const { requestMock } = vi.hoisted(() => ({ requestMock: vi.fn() }));

vi.mock("./bridge", () => ({ aoBridge: { remoteDaemon: { request: requestMock } } }));

import { remoteDaemonFetch } from "./remote-daemon-fetch";

describe("remoteDaemonFetch", () => {
	beforeEach(() => {
		requestMock.mockReset();
		requestMock.mockResolvedValue({
			status: 200,
			headers: { "content-type": "application/json" },
			body: '{"ok":true}',
		});
	});

	it("forwards the daemon path, method, and body through main", async () => {
		const response = await remoteDaemonFetch(
			new Request("http://127.0.0.1:3001/api/v1/sessions?limit=2", {
				method: "POST",
				headers: { "content-type": "application/json" },
				body: '{"prompt":"hi"}',
			}),
		);
		expect(requestMock).toHaveBeenCalledWith({
			path: "/api/v1/sessions?limit=2",
			method: "POST",
			headers: { "content-type": "application/json" },
			body: '{"prompt":"hi"}',
		});
		expect(response.status).toBe(200);
		await expect(response.json()).resolves.toEqual({ ok: true });
	});

	it("drops headers main owns, including any renderer-supplied bearer", async () => {
		await remoteDaemonFetch(
			new Request("http://127.0.0.1:3001/api/v1/projects", {
				headers: {
					authorization: "Bearer stolen",
					cookie: "session=1",
					origin: "app://renderer",
					accept: "application/json",
				},
			}),
		);
		expect(requestMock).toHaveBeenCalledWith(
			expect.objectContaining({ headers: { accept: "application/json" } }),
		);
	});

	it("sends no body for GET and drops a body the daemon cannot return", async () => {
		requestMock.mockResolvedValue({ status: 204, headers: {}, body: "" });
		const response = await remoteDaemonFetch(new Request("http://127.0.0.1:3001/api/v1/sessions"));
		expect(requestMock).toHaveBeenCalledWith(expect.objectContaining({ body: undefined }));
		expect(response.status).toBe(204);
		expect(response.body).toBeNull();
	});

	it("preserves the daemon's error envelope so callers can read the code", async () => {
		requestMock.mockResolvedValue({
			status: 403,
			headers: { "content-type": "application/json" },
			body: '{"error":"forbidden","code":"NOPE","message":"no"}',
		});
		const response = await remoteDaemonFetch(new Request("http://127.0.0.1:3001/api/v1/projects"));
		expect(response.status).toBe(403);
		await expect(response.json()).resolves.toEqual({ error: "forbidden", code: "NOPE", message: "no" });
	});

	it("decodes base64 bodies for binary responses", async () => {
		const png = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x00, 0xff]);
		requestMock.mockResolvedValue({
			status: 200,
			headers: { "content-type": "image/png" },
			body: Buffer.from(png).toString("base64"),
			bodyEncoding: "base64",
		});
		const response = await remoteDaemonFetch(
			new Request("http://127.0.0.1:3001/api/v1/sessions/ao-1/workspace/file/blob?path=a.png"),
		);
		expect(new Uint8Array(await response.arrayBuffer())).toEqual(png);
	});

	it("aborts without calling main when the signal is already aborted", async () => {
		const controller = new AbortController();
		controller.abort();
		await expect(
			remoteDaemonFetch(new Request("http://127.0.0.1:3001/api/v1/sessions", { signal: controller.signal })),
		).rejects.toMatchObject({ name: "AbortError" });
		expect(requestMock).not.toHaveBeenCalled();
	});
});
