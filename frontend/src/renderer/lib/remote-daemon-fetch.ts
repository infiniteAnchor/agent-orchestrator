// Renderer-side `fetch` replacement for remote mode. The typed API client keeps
// building requests against the public base URL; this module forwards the path
// and body to Electron main, which resolves the enrolled server, verifies the
// pinned host id, and attaches the LAN bearer the renderer never sees.

import { aoBridge } from "./bridge";

// Headers main must own or must not forward: it discards an
// Authorization/Cookie/Host a compromised renderer supplies anyway, but there is
// no reason to ship them across IPC.
const DROPPED_REQUEST_HEADERS = new Set(["authorization", "cookie", "host", "origin", "referer"]);

function abortError(): DOMException {
	return new DOMException("The request was aborted.", "AbortError");
}

/** Decode the main-process base64 transport for binary response bodies. */
function base64ToBytes(encoded: string): ArrayBuffer {
	const binary = atob(encoded);
	const bytes = new Uint8Array(binary.length);
	for (let i = 0; i < binary.length; i += 1) bytes[i] = binary.charCodeAt(i);
	return bytes.buffer as ArrayBuffer;
}

function forwardableHeaders(input: Request): Record<string, string> {
	const headers: Record<string, string> = {};
	input.headers.forEach((value, key) => {
		if (!DROPPED_REQUEST_HEADERS.has(key.toLowerCase())) headers[key] = value;
	});
	return headers;
}

/** Perform a daemon request through the main-process remote proxy. */
export async function remoteDaemonFetch(input: Request): Promise<Response> {
	if (input.signal.aborted) throw abortError();
	const url = new URL(input.url);
	const body =
		input.method === "GET" || input.method === "HEAD" ? undefined : await input.text();
	if (input.signal.aborted) throw abortError();
	const proxied = await aoBridge.remoteDaemon.request({
		path: `${url.pathname}${url.search}`,
		method: input.method,
		headers: forwardableHeaders(input),
		body,
	});
	if (input.signal.aborted) throw abortError();
	// A 204/304 response must not carry a body, and openapi-fetch treats a
	// non-null body with those statuses as an error.
	const hasBody = proxied.status !== 204 && proxied.status !== 304 && proxied.body !== "";
	const responseBody =
		!hasBody ? null : proxied.bodyEncoding === "base64" ? base64ToBytes(proxied.body) : proxied.body;
	return new Response(responseBody, {
		status: proxied.status,
		headers: proxied.headers,
	});
}
