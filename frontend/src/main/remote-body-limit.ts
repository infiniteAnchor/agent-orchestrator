// Shared byte-limited response body reader for remote desktop transports.

/** Cap for unauthenticated identity JSON (`{ hostId, apiVersion }`). */
export const REMOTE_IDENTITY_BODY_LIMIT = 64 << 10; // 64 KiB

/** Cap for non-2xx SSE open error envelopes. */
export const REMOTE_DAEMON_ERROR_BODY_LIMIT = 64 << 10; // 64 KiB

/** Cap for buffered HTTP bodies before they cross IPC into the renderer. */
export const REMOTE_DAEMON_RESPONSE_BODY_LIMIT = 16 << 20; // 16 MiB

export class RemoteBodyLimitError extends Error {
	readonly limit: number;

	constructor(limit: number) {
		super(`response body exceeds the ${limit}-byte limit.`);
		this.name = "RemoteBodyLimitError";
		this.limit = limit;
	}
}

/** Read a fetch Response body with a hard byte ceiling. */
export async function readResponseBodyLimitedBytes(response: Response, limit: number): Promise<Uint8Array> {
	const contentLength = response.headers.get("content-length");
	if (contentLength !== null) {
		const declared = Number(contentLength);
		if (Number.isFinite(declared) && declared > limit) {
			throw new RemoteBodyLimitError(limit);
		}
	}
	if (response.body === null) {
		return new Uint8Array(0);
	}
	const reader = response.body.getReader();
	const chunks: Uint8Array[] = [];
	let total = 0;
	try {
		for (;;) {
			const { done, value } = await reader.read();
			if (done) break;
			if (value === undefined || value.byteLength === 0) continue;
			total += value.byteLength;
			if (total > limit) {
				await reader.cancel().catch(() => undefined);
				throw new RemoteBodyLimitError(limit);
			}
			chunks.push(value);
		}
	} finally {
		reader.releaseLock();
	}
	if (chunks.length === 0) return new Uint8Array(0);
	if (chunks.length === 1) return chunks[0]!;
	const merged = new Uint8Array(total);
	let offset = 0;
	for (const chunk of chunks) {
		merged.set(chunk, offset);
		offset += chunk.byteLength;
	}
	return merged;
}

/** Read a fetch Response body as UTF-8 text with a hard byte ceiling. */
export async function readResponseBodyLimited(response: Response, limit: number): Promise<string> {
	const bytes = await readResponseBodyLimitedBytes(response, limit);
	return bytes.byteLength === 0 ? "" : Buffer.from(bytes).toString("utf8");
}
