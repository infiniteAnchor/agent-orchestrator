// Electron-main transport for remote AO daemon calls (Phase 2).
//
// The LAN bearer stays in main. The renderer sends { path, method, ... } over
// IPC; main resolves the enrolled endpoint + password, verifies the pinned
// host id via GET /api/v1/identity (no bearer), then attaches Authorization.
// A renderer-supplied Authorization header is discarded.

import { ipcMain } from "electron";
import { randomUUID } from "node:crypto";
import {
	REMOTE_DAEMON_ERROR_BODY_LIMIT,
	REMOTE_DAEMON_RESPONSE_BODY_LIMIT,
	RemoteBodyLimitError,
	readResponseBodyLimited,
} from "./remote-body-limit";
import {
	getActiveRemoteProfile,
	type RemoteServerProfile,
} from "./remote-connection-store";
import {
	verifyPinnedIdentity,
	type IdentityGateResult,
} from "./remote-identity-gate";

export const REMOTE_DAEMON_REQUEST_CHANNEL = "remoteDaemon:request";
export const REMOTE_DAEMON_OPEN_STREAM_CHANNEL = "remoteDaemon:openStream";
export const REMOTE_DAEMON_CLOSE_STREAM_CHANNEL = "remoteDaemon:closeStream";

export function remoteDaemonStreamChannel(streamId: string): string {
	return `remoteDaemon:stream:${streamId}`;
}

export const MAX_STREAMS_PER_WEBCONTENTS = 8;

export { REMOTE_DAEMON_RESPONSE_BODY_LIMIT };

export const REMOTE_DAEMON_STREAM_ERROR_MARKER = "REMOTE_DAEMON_STREAM_ERROR";

/** Paths the remote desktop may call. Everything else is rejected before fetch. */
const ALLOWED_EXACT = new Set(["/healthz", "/readyz"]);
const API_PREFIX = "/api/v1";

/** Loopback-only control prefixes — never proxy these even if enrolled. */
const BLOCKED_PREFIXES = [
	"/api/v1/desktop",
	"/api/v1/mobile",
	"/api/v1/dev",
	"/api/v1/browser",
	"/api/v1/system/install",
	"/api/v1/agents/codex",
	"/shutdown",
	"/internal/",
];

export interface RemoteDaemonProxyRequestInit {
	path: string;
	method: string;
	headers?: Record<string, string>;
	body?: string;
}

export interface RemoteDaemonProxyResponse {
	status: number;
	headers: Record<string, string>;
	body: string;
}

export type RemoteDaemonStreamEvent =
	| { type: "chunk"; data: string }
	| { type: "end" }
	| { type: "error"; message: string };

export interface RemoteDaemonStreamSender {
	readonly id: number;
	isDestroyed(): boolean;
	send(channel: string, event: RemoteDaemonStreamEvent): void;
	once(event: "destroyed", listener: () => void): unknown;
}

export interface RemoteDaemonProxy {
	request(init: unknown): Promise<RemoteDaemonProxyResponse>;
	openStream(sender: RemoteDaemonStreamSender, init: unknown): Promise<{ streamId: string }>;
	closeStream(sender: Pick<RemoteDaemonStreamSender, "id">, streamId: unknown): void;
}

export interface RemoteDaemonProxyOptions {
	fetchImpl?: typeof fetch;
	getActiveProfile?: () => Promise<RemoteServerProfile | null>;
	verifyIdentity?: (options: {
		baseUrl: string;
		pinnedHostId: string;
		fetchImpl?: typeof fetch;
		signal?: AbortSignal;
	}) => Promise<IdentityGateResult>;
}

function invalid(detail: string): Error {
	return new Error(`Invalid remote daemon request: ${detail}`);
}

function streamError(status: number, detail: string): Error {
	return new Error(`${REMOTE_DAEMON_STREAM_ERROR_MARKER} ${status} ${detail}`);
}

function pathAllowed(method: string, path: string): boolean {
	const pathname = path.split("?", 1)[0] ?? path;
	if (ALLOWED_EXACT.has(pathname)) return true;
	if (!(pathname === API_PREFIX || pathname.startsWith(`${API_PREFIX}/`))) {
		return false;
	}
	for (const prefix of BLOCKED_PREFIXES) {
		const trimmed = prefix.endsWith("/") ? prefix.slice(0, -1) : prefix;
		if (pathname === trimmed || pathname.startsWith(`${trimmed}/`)) {
			return false;
		}
	}
	// Host-mutating agent install stays loopback-only (matches LAN blocklist).
	if (
		method.toUpperCase() === "POST" &&
		pathname.startsWith("/api/v1/agents/") &&
		pathname.endsWith("/install")
	) {
		return false;
	}
	if (pathname.startsWith("/api/v1/sessions/") && pathname.endsWith("/preview/server")) {
		return false;
	}
	return true;
}

/** Collapse dot segments so /api/v1/../shutdown cannot pass the allowlist. */
function normalizeRequestPath(path: string): string {
	if (!path.startsWith("/")) throw invalid("path must be absolute on the daemon (start with /).");
	const url = new URL(path, "http://ao.invalid");
	return `${url.pathname}${url.search}`;
}

function validateInit(init: unknown): RemoteDaemonProxyRequestInit {
	if (typeof init !== "object" || init === null) {
		throw invalid("the request payload must be an object.");
	}
	const { path, method, headers, body } = init as Record<string, unknown>;
	if (typeof path !== "string" || path === "") throw invalid("path must be a non-empty string.");
	if (typeof method !== "string" || !/^[A-Za-z]+$/.test(method)) {
		throw invalid("method must be an HTTP method token.");
	}
	if (body !== undefined && typeof body !== "string") throw invalid("body must be a string when present.");
	if (headers !== undefined) {
		if (typeof headers !== "object" || headers === null || Array.isArray(headers)) {
			throw invalid("headers must be a plain record of strings.");
		}
		for (const value of Object.values(headers)) {
			if (typeof value !== "string") throw invalid("headers must be a plain record of strings.");
		}
	}
	const normalizedPath = normalizeRequestPath(path);
	if (!pathAllowed(method, normalizedPath)) {
		throw invalid(`path ${JSON.stringify(path)} is not allowed for remote desktop.`);
	}
	return {
		path: normalizedPath,
		method,
		headers: headers as Record<string, string> | undefined,
		body: body as string | undefined,
	};
}

function resolveTargetUrl(baseUrl: string, requestPath: string): string {
	const base = new URL(baseUrl);
	const basePath = base.pathname.replace(/\/+$/, "");
	const target = new URL(base.origin + basePath + requestPath);
	if (target.origin !== base.origin) {
		throw invalid("path must stay on the enrolled origin after URL normalization.");
	}
	const pathname = target.pathname;
	const relative = pathname.slice(basePath.length) || "/";
	if (!pathAllowed("GET", relative.split("?", 1)[0] ?? relative)) {
		// Re-check after WHATWG normalization so /api/v1/../shutdown cannot escape.
		throw invalid(`path must stay under an allowed prefix after URL normalization.`);
	}
	return target.toString();
}

function buildHeaders(requested: Record<string, string> | undefined, password: string): Headers {
	const headers = new Headers();
	for (const [name, value] of Object.entries(requested ?? {})) {
		const lower = name.toLowerCase();
		if (lower === "authorization" || lower === "cookie" || lower === "host") continue;
		headers.set(name, value);
	}
	headers.set("Authorization", `Bearer ${password}`);
	return headers;
}

function noRemoteProfileResponse(): RemoteDaemonProxyResponse {
	return {
		status: 503,
		headers: { "content-type": "application/json" },
		body: JSON.stringify({
			error: "unavailable",
			code: "NO_REMOTE_PROFILE",
			message: "No remote AO server is selected. Enroll a server or switch back to local mode.",
		}),
	};
}

function identityFailureResponse(gate: Extract<IdentityGateResult, { ok: false }>): RemoteDaemonProxyResponse {
	const status = gate.reason === "host_mismatch" ? 403 : 502;
	return {
		status,
		headers: { "content-type": "application/json" },
		body: JSON.stringify({
			error: "identity_failed",
			code: gate.reason.toUpperCase(),
			message: gate.message,
			observedHostId: gate.observedHostId,
		}),
	};
}

async function envelopeMessage(response: Response): Promise<string> {
	const fallback = `Remote daemon stream request failed with status ${response.status}.`;
	try {
		const text = await readResponseBodyLimited(response, REMOTE_DAEMON_ERROR_BODY_LIMIT);
		const body = JSON.parse(text) as { message?: unknown; error?: unknown } | null;
		if (typeof body?.message === "string" && body.message !== "") return body.message;
		if (typeof body?.error === "string" && body.error !== "") return body.error;
	} catch {
		// Non-JSON / oversized body: keep the status-derived message.
	}
	return fallback;
}

interface ActiveStream {
	controller: AbortController;
	sender: RemoteDaemonStreamSender;
	channel: string;
	closed: boolean;
}

interface PendingStreamOpen {
	controller: AbortController;
	cancelled: boolean;
}

export function createRemoteDaemonProxy(
	getStateDir: () => string,
	options: RemoteDaemonProxyOptions = {},
): RemoteDaemonProxy {
	const doFetch: typeof fetch = options.fetchImpl ?? ((input, init) => fetch(input, init));
	const getActiveProfile =
		options.getActiveProfile ?? (() => getActiveRemoteProfile(getStateDir()));
	const verifyIdentity = options.verifyIdentity ?? verifyPinnedIdentity;

	const streams = new Map<string, ActiveStream>();
	const pendingOpens = new Map<string, PendingStreamOpen>();
	const streamIdsBySender = new Map<number, Set<string>>();

	function teardown(streamId: string, stream: ActiveStream): void {
		stream.closed = true;
		streams.delete(streamId);
		pendingOpens.delete(streamId);
		streamIdsBySender.get(stream.sender.id)?.delete(streamId);
		stream.controller.abort();
	}

	function cancelPendingOpen(streamId: string): void {
		const pending = pendingOpens.get(streamId);
		if (pending === undefined) return;
		pending.cancelled = true;
		pending.controller.abort();
		pendingOpens.delete(streamId);
	}

	function emit(stream: ActiveStream, event: RemoteDaemonStreamEvent): void {
		if (stream.closed || stream.sender.isDestroyed()) return;
		stream.sender.send(stream.channel, event);
	}

	function trackSender(sender: RemoteDaemonStreamSender): Set<string> {
		let ids = streamIdsBySender.get(sender.id);
		if (ids === undefined) {
			ids = new Set();
			streamIdsBySender.set(sender.id, ids);
			sender.once("destroyed", () => {
				const orphaned = streamIdsBySender.get(sender.id);
				streamIdsBySender.delete(sender.id);
				for (const streamId of orphaned ?? []) {
					cancelPendingOpen(streamId);
					const stream = streams.get(streamId);
					if (stream !== undefined) teardown(streamId, stream);
					else ids?.delete(streamId);
				}
			});
		}
		return ids;
	}

	async function pump(streamId: string, stream: ActiveStream, body: ReadableStream<Uint8Array>): Promise<void> {
		const reader = body.getReader();
		const decoder = new TextDecoder();
		try {
			for (;;) {
				const { done, value } = await reader.read();
				if (value !== undefined) {
					const data = decoder.decode(value, { stream: true });
					if (data !== "") emit(stream, { type: "chunk", data });
				}
				if (done) break;
			}
			const tail = decoder.decode();
			if (tail !== "") emit(stream, { type: "chunk", data: tail });
			emit(stream, { type: "end" });
		} catch (error) {
			emit(stream, {
				type: "error",
				message: error instanceof Error ? error.message : String(error),
			});
		} finally {
			teardown(streamId, stream);
			reader.releaseLock();
		}
	}

	async function authorize(): Promise<
		| { ok: true; profile: RemoteServerProfile }
		| { ok: false; response: RemoteDaemonProxyResponse }
	> {
		const profile = await getActiveProfile();
		if (profile === null) return { ok: false, response: noRemoteProfileResponse() };
		const gate = await verifyIdentity({
			baseUrl: profile.baseUrl,
			pinnedHostId: profile.pinnedHostId,
			fetchImpl: doFetch,
		});
		if (!gate.ok) return { ok: false, response: identityFailureResponse(gate) };
		return { ok: true, profile };
	}

	async function request(init: unknown): Promise<RemoteDaemonProxyResponse> {
		const valid = validateInit(init);
		const auth = await authorize();
		if (!auth.ok) return auth.response;
		const url = resolveTargetUrl(auth.profile.baseUrl, valid.path);
		const response = await doFetch(url, {
			method: valid.method.toUpperCase(),
			headers: buildHeaders(valid.headers, auth.profile.password),
			body: valid.body,
			// Authenticated requests must not follow redirects off the enrolled origin.
			redirect: "manual",
		});
		if (response.type === "opaqueredirect" || (response.status >= 300 && response.status < 400)) {
			return {
				status: 502,
				headers: { "content-type": "application/json" },
				body: JSON.stringify({
					error: "redirect",
					code: "REDIRECT",
					message: "Remote daemon response redirected; the request was not followed.",
				}),
			};
		}
		const headers: Record<string, string> = {};
		response.headers.forEach((value, key) => {
			headers[key] = value;
		});
		try {
			return {
				status: response.status,
				headers,
				body: await readResponseBodyLimited(response, REMOTE_DAEMON_RESPONSE_BODY_LIMIT),
			};
		} catch (err) {
			if (err instanceof RemoteBodyLimitError) {
				throw invalid(`response body exceeds the ${REMOTE_DAEMON_RESPONSE_BODY_LIMIT}-byte IPC limit.`);
			}
			throw err;
		}
	}

	async function openStream(
		sender: RemoteDaemonStreamSender,
		init: unknown,
	): Promise<{ streamId: string }> {
		const valid = validateInit(init);
		const ids = trackSender(sender);
		if (ids.size >= MAX_STREAMS_PER_WEBCONTENTS) {
			throw streamError(
				429,
				`This window already has ${MAX_STREAMS_PER_WEBCONTENTS} open remote daemon streams.`,
			);
		}
		// Reserve capacity before any await so concurrent openStream calls cannot
		// all pass the cap while identity/fetch are in flight.
		const streamId = randomUUID();
		ids.add(streamId);
		const controller = new AbortController();
		const pending: PendingStreamOpen = { controller, cancelled: false };
		pendingOpens.set(streamId, pending);
		const releaseReservation = (): void => {
			cancelPendingOpen(streamId);
			ids.delete(streamId);
		};

		try {
			const auth = await authorize();
			if (pending.cancelled || sender.isDestroyed()) {
				throw streamError(499, "Remote daemon stream open cancelled.");
			}
			if (!auth.ok) {
				let detail = "Remote daemon authorization failed.";
				try {
					const parsed = JSON.parse(auth.response.body) as { message?: unknown };
					if (typeof parsed.message === "string" && parsed.message !== "") detail = parsed.message;
				} catch {
					// keep default detail
				}
				throw streamError(auth.response.status, detail);
			}
			const url = resolveTargetUrl(auth.profile.baseUrl, valid.path);
			const response = await doFetch(url, {
				method: valid.method.toUpperCase(),
				headers: buildHeaders(valid.headers, auth.profile.password),
				body: valid.body,
				signal: controller.signal,
				redirect: "manual",
			});
			if (pending.cancelled || sender.isDestroyed()) {
				throw streamError(499, "Remote daemon stream open cancelled.");
			}
			if (response.type === "opaqueredirect" || (response.status >= 300 && response.status < 400)) {
				throw streamError(502, "Remote daemon stream redirected; the request was not followed.");
			}
			if (!response.ok) {
				throw streamError(response.status, await envelopeMessage(response));
			}
			if (response.body === null) {
				throw streamError(response.status, "The event stream response has no body.");
			}

			pendingOpens.delete(streamId);
			const stream: ActiveStream = {
				controller,
				sender,
				channel: remoteDaemonStreamChannel(streamId),
				closed: false,
			};
			streams.set(streamId, stream);
			const body = response.body;
			setImmediate(() => {
				void pump(streamId, stream, body);
			});
			return { streamId };
		} catch (error) {
			releaseReservation();
			throw error;
		}
	}

	function closeStream(sender: Pick<RemoteDaemonStreamSender, "id">, streamId: unknown): void {
		if (typeof streamId !== "string") return;
		const stream = streams.get(streamId);
		if (stream === undefined || stream.sender.id !== sender.id) return;
		teardown(streamId, stream);
	}

	return { request, openStream, closeStream };
}

export function installRemoteDaemonProxy(getStateDir: () => string): void {
	const proxy = createRemoteDaemonProxy(getStateDir);
	ipcMain.handle(REMOTE_DAEMON_REQUEST_CHANNEL, (_event, init: unknown) => proxy.request(init));
	ipcMain.handle(REMOTE_DAEMON_OPEN_STREAM_CHANNEL, (event, init: unknown) =>
		proxy.openStream(event.sender, init),
	);
	ipcMain.on(REMOTE_DAEMON_CLOSE_STREAM_CHANNEL, (event, streamId: unknown) => {
		proxy.closeStream(event.sender, streamId);
	});
}
