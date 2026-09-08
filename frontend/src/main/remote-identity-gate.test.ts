import { describe, expect, it, vi } from "vitest";
import { verifyPinnedIdentity } from "./remote-identity-gate";

function jsonResponse(body: unknown, status = 200): Response {
	return new Response(JSON.stringify(body), {
		status,
		headers: { "content-type": "application/json" },
	});
}

describe("verifyPinnedIdentity", () => {
	it("succeeds when hostId matches the pin", async () => {
		const fetchImpl = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
			expect(String(input)).toBe("http://100.64.0.1:3011/api/v1/identity");
			expect(init?.method).toBe("GET");
			expect((init?.headers as Record<string, string>).Accept).toBe("application/json");
			expect(init?.credentials).toBe("omit");
			expect(init?.redirect).toBe("manual");
			const headers = new Headers(init?.headers);
			expect(headers.get("authorization")).toBeNull();
			return jsonResponse({ hostId: "h_abc", apiVersion: 1 });
		});

		const result = await verifyPinnedIdentity({
			baseUrl: "http://100.64.0.1:3011",
			pinnedHostId: "h_abc",
			fetchImpl: fetchImpl as unknown as typeof fetch,
		});
		expect(result).toEqual({ ok: true, hostId: "h_abc", apiVersion: 1 });
	});

	it("rejects redirects without sending a bearer", async () => {
		const fetchImpl = vi.fn(async () =>
			new Response(null, {
				status: 302,
				headers: { Location: "http://genuine.example:3011/api/v1/identity" },
			}),
		);
		const result = await verifyPinnedIdentity({
			baseUrl: "http://attacker.example:3011",
			pinnedHostId: "h_abc",
			fetchImpl: fetchImpl as unknown as typeof fetch,
		});
		expect(result.ok).toBe(false);
		if (result.ok) return;
		expect(result.reason).toBe("redirect");
		expect(result.message).toMatch(/not sent/i);
		expect(fetchImpl).toHaveBeenCalledWith(
			expect.any(String),
			expect.objectContaining({ redirect: "manual" }),
		);
	});

	it("fails closed on host mismatch without implying a bearer was sent", async () => {
		const fetchImpl = vi.fn(async () => jsonResponse({ hostId: "h_other", apiVersion: 1 }));
		const result = await verifyPinnedIdentity({
			baseUrl: "http://192.168.1.10:3011",
			pinnedHostId: "h_expected",
			fetchImpl: fetchImpl as unknown as typeof fetch,
		});
		expect(result.ok).toBe(false);
		if (result.ok) return;
		expect(result.reason).toBe("host_mismatch");
		expect(result.observedHostId).toBe("h_other");
		expect(result.message).toMatch(/not sent/i);
	});

	it("rejects an empty pin before fetching", async () => {
		const fetchImpl = vi.fn();
		const result = await verifyPinnedIdentity({
			baseUrl: "http://127.0.0.1:3011",
			pinnedHostId: "  ",
			fetchImpl: fetchImpl as unknown as typeof fetch,
		});
		expect(result).toMatchObject({ ok: false, reason: "missing_pin" });
		expect(fetchImpl).not.toHaveBeenCalled();
	});

	it("treats a non-200 probe as failure", async () => {
		const fetchImpl = vi.fn(async () => jsonResponse({ error: "nope" }, 503));
		const result = await verifyPinnedIdentity({
			baseUrl: "http://example.test:3011",
			pinnedHostId: "h_abc",
			fetchImpl: fetchImpl as unknown as typeof fetch,
		});
		expect(result).toMatchObject({ ok: false, reason: "bad_status", status: 503 });
	});
});
