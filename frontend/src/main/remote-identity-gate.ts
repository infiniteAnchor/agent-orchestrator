// Pre-auth identity gate for remote AO servers.
//
// Calls unauthenticated GET /api/v1/identity and compares hostId to the pinned
// value before any bearer is attached. A mismatch fails closed.

export type IdentityGateOk = {
	ok: true;
	hostId: string;
	apiVersion: number;
};

export type IdentityGateFail = {
	ok: false;
	reason:
		| "fetch_failed"
		| "bad_status"
		| "redirect"
		| "invalid_body"
		| "host_mismatch"
		| "missing_pin";
	message: string;
	observedHostId?: string;
	status?: number;
};

export type IdentityGateResult = IdentityGateOk | IdentityGateFail;

export type IdentityGateOptions = {
	baseUrl: string;
	pinnedHostId: string;
	fetchImpl?: typeof fetch;
	/** Abort/timeout signal for the probe. */
	signal?: AbortSignal;
};

function identityUrl(baseUrl: string): string {
	const base = new URL(baseUrl);
	base.pathname = `${base.pathname.replace(/\/+$/, "")}/api/v1/identity`;
	base.search = "";
	base.hash = "";
	return base.toString();
}

/**
 * Probe the remote identity endpoint with no Authorization header and compare
 * to the pinned host id. Callers must not send a bearer when this returns ok:false.
 */
export async function verifyPinnedIdentity(options: IdentityGateOptions): Promise<IdentityGateResult> {
	const pinnedHostId = options.pinnedHostId.trim();
	if (!pinnedHostId) {
		return {
			ok: false,
			reason: "missing_pin",
			message: "No pinned host id is configured for this remote server.",
		};
	}

	const doFetch = options.fetchImpl ?? ((input, init) => fetch(input, init));
	const probeUrl = identityUrl(options.baseUrl);
	let response: Response;
	try {
		response = await doFetch(probeUrl, {
			method: "GET",
			headers: { Accept: "application/json" },
			signal: options.signal,
			// Explicitly omit credentials so a browser-ish fetch never attaches cookies.
			credentials: "omit",
			// Never follow redirects: a hijacked enrolled address could otherwise
			// bounce this probe to the genuine pinned host, match hostId, then
			// receive the bearer on the original (attacker-controlled) origin.
			redirect: "manual",
		});
	} catch (err) {
		return {
			ok: false,
			reason: "fetch_failed",
			message: err instanceof Error ? err.message : "Identity probe failed.",
		};
	}

	if (response.type === "opaqueredirect" || (response.status >= 300 && response.status < 400)) {
		return {
			ok: false,
			reason: "redirect",
			status: response.status,
			message:
				"Identity probe followed or returned a redirect. The connection secret was not sent.",
		};
	}

	// Fail closed if the runtime rewrote the final URL away from the enrolled origin.
	if (response.url) {
		try {
			const expected = new URL(probeUrl);
			const actual = new URL(response.url);
			if (actual.origin !== expected.origin || actual.pathname !== expected.pathname) {
				return {
					ok: false,
					reason: "redirect",
					message:
						"Identity probe resolved to a different origin or path. The connection secret was not sent.",
				};
			}
		} catch {
			return {
				ok: false,
				reason: "redirect",
				message:
					"Identity probe returned an unparseable final URL. The connection secret was not sent.",
			};
		}
	}

	if (!response.ok) {
		return {
			ok: false,
			reason: "bad_status",
			status: response.status,
			message: `Identity probe returned HTTP ${response.status}.`,
		};
	}

	let body: unknown;
	try {
		body = await response.json();
	} catch {
		return {
			ok: false,
			reason: "invalid_body",
			message: "Identity probe returned non-JSON.",
		};
	}
	if (typeof body !== "object" || body === null) {
		return {
			ok: false,
			reason: "invalid_body",
			message: "Identity probe body must be an object.",
		};
	}
	const record = body as Record<string, unknown>;
	const hostId = typeof record.hostId === "string" ? record.hostId : "";
	const apiVersion = typeof record.apiVersion === "number" ? record.apiVersion : NaN;
	if (!hostId || !Number.isFinite(apiVersion)) {
		return {
			ok: false,
			reason: "invalid_body",
			message: "Identity probe must include hostId and apiVersion.",
			observedHostId: hostId || undefined,
		};
	}

	if (hostId !== pinnedHostId) {
		return {
			ok: false,
			reason: "host_mismatch",
			message:
				"Remote host id does not match the pinned value. The connection secret was not sent.",
			observedHostId: hostId,
		};
	}

	return { ok: true, hostId, apiVersion };
}
