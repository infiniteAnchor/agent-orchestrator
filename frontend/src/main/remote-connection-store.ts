// Persists remote AO server enrollment under ~/.ao for Phase 2 desktop mode.
// The connection password never returns to the renderer after enrollment.

import { randomUUID } from "node:crypto";
import { chmod, mkdir, readFile, rename, writeFile } from "node:fs/promises";
import path from "node:path";
import type {
	ConnectionMode,
	EnrollRemoteServerInput,
	RemoteServerProfilePublic,
} from "../shared/remote-connection";

export const REMOTE_CONNECTION_FILE_NAME = "remote-servers.json";

export type RemoteServerProfile = RemoteServerProfilePublic & {
	/** Shared LAN bearer password. Main-process only. */
	password: string;
};

export type RemoteConnectionStore = {
	active: ConnectionMode;
	profiles: RemoteServerProfile[];
};

export type PublicRemoteConnectionStore = {
	active: ConnectionMode;
	profiles: RemoteServerProfilePublic[];
};

const emptyStore = (): RemoteConnectionStore => ({
	active: { kind: "local" },
	profiles: [],
});

let storeQueue: Promise<void> = Promise.resolve();

function runStoreOp<T>(operation: () => Promise<T>): Promise<T> {
	const queued = storeQueue.then(operation, operation);
	storeQueue = queued.then(
		() => undefined,
		() => undefined,
	);
	return queued;
}

function storePath(stateDir: string): string {
	return path.join(stateDir, REMOTE_CONNECTION_FILE_NAME);
}

function publicProfile(profile: RemoteServerProfile): RemoteServerProfilePublic {
	const { password: _password, ...rest } = profile;
	return rest;
}

export function toPublicStore(store: RemoteConnectionStore): PublicRemoteConnectionStore {
	return {
		active: store.active,
		profiles: store.profiles.map(publicProfile),
	};
}

function normalizeBaseUrl(raw: string): string {
	const trimmed = raw.trim();
	let url: URL;
	try {
		url = new URL(trimmed);
	} catch {
		throw new Error("baseUrl must be a valid URL.");
	}
	if (url.protocol !== "http:" && url.protocol !== "https:") {
		throw new Error("baseUrl must use http or https.");
	}
	if (url.username !== "" || url.password !== "" || url.search !== "" || url.hash !== "") {
		throw new Error("baseUrl must not carry credentials, a query string, or a fragment.");
	}
	// Strip trailing slash so path joins stay predictable.
	return `${url.origin}${url.pathname.replace(/\/+$/, "")}` || url.origin;
}

function normalizeHostId(raw: string): string {
	const hostId = raw.trim();
	if (!hostId) throw new Error("pinnedHostId must be non-empty.");
	return hostId;
}

function normalizePassword(raw: string): string {
	if (typeof raw !== "string" || raw === "") {
		throw new Error("password must be a non-empty string.");
	}
	return raw;
}

function coerceStore(value: unknown): RemoteConnectionStore {
	if (typeof value !== "object" || value === null) return emptyStore();
	const record = value as Record<string, unknown>;
	const activeRaw = record.active;
	let active: ConnectionMode = { kind: "local" };
	if (typeof activeRaw === "object" && activeRaw !== null) {
		const a = activeRaw as Record<string, unknown>;
		if (a.kind === "local") active = { kind: "local" };
		else if (a.kind === "remote" && typeof a.profileId === "string" && a.profileId !== "") {
			active = { kind: "remote", profileId: a.profileId };
		}
	}
	const profiles: RemoteServerProfile[] = [];
	if (Array.isArray(record.profiles)) {
		for (const item of record.profiles) {
			if (typeof item !== "object" || item === null) continue;
			const p = item as Record<string, unknown>;
			if (
				typeof p.id !== "string" ||
				typeof p.label !== "string" ||
				typeof p.baseUrl !== "string" ||
				typeof p.pinnedHostId !== "string" ||
				typeof p.password !== "string" ||
				typeof p.createdAt !== "string" ||
				typeof p.updatedAt !== "string"
			) {
				continue;
			}
			profiles.push({
				id: p.id,
				label: p.label,
				baseUrl: p.baseUrl,
				pinnedHostId: p.pinnedHostId,
				password: p.password,
				createdAt: p.createdAt,
				updatedAt: p.updatedAt,
			});
		}
	}
	if (active.kind === "remote") {
		const remoteId = active.profileId;
		if (!profiles.some((p) => p.id === remoteId)) {
			active = { kind: "local" };
		}
	}
	return { active, profiles };
}

async function readStoreUnlocked(stateDir: string): Promise<RemoteConnectionStore> {
	try {
		const raw = await readFile(storePath(stateDir), "utf8");
		return coerceStore(JSON.parse(raw));
	} catch {
		return emptyStore();
	}
}

async function writeStoreUnlocked(stateDir: string, store: RemoteConnectionStore): Promise<void> {
	await mkdir(stateDir, { recursive: true, mode: 0o750 });
	const file = storePath(stateDir);
	const tmp = path.join(stateDir, `.remote-servers-${process.pid}-${Date.now()}.json`);
	const data = `${JSON.stringify(store, null, 2)}\n`;
	await writeFile(tmp, data, { mode: 0o600 });
	await chmod(tmp, 0o600);
	await rename(tmp, file);
	await chmod(file, 0o600);
}

export async function readRemoteConnectionStore(stateDir: string): Promise<RemoteConnectionStore> {
	return runStoreOp(() => readStoreUnlocked(stateDir));
}

export async function readPublicRemoteConnectionStore(
	stateDir: string,
): Promise<PublicRemoteConnectionStore> {
	return toPublicStore(await readRemoteConnectionStore(stateDir));
}

export async function enrollRemoteServer(
	stateDir: string,
	input: EnrollRemoteServerInput,
): Promise<PublicRemoteConnectionStore> {
	return runStoreOp(async () => {
		const store = await readStoreUnlocked(stateDir);
		const now = new Date().toISOString();
		const profile: RemoteServerProfile = {
			id: randomUUID(),
			label: input.label.trim() || "Remote AO",
			baseUrl: normalizeBaseUrl(input.baseUrl),
			pinnedHostId: normalizeHostId(input.pinnedHostId),
			password: normalizePassword(input.password),
			createdAt: now,
			updatedAt: now,
		};
		store.profiles.push(profile);
		store.active = { kind: "remote", profileId: profile.id };
		await writeStoreUnlocked(stateDir, store);
		return toPublicStore(store);
	});
}

export async function setActiveConnectionMode(
	stateDir: string,
	mode: ConnectionMode,
): Promise<PublicRemoteConnectionStore> {
	return runStoreOp(async () => {
		const store = await readStoreUnlocked(stateDir);
		if (mode.kind === "remote") {
			if (!store.profiles.some((p) => p.id === mode.profileId)) {
				throw new Error(`Unknown remote profile ${mode.profileId}.`);
			}
		}
		store.active = mode;
		await writeStoreUnlocked(stateDir, store);
		return toPublicStore(store);
	});
}

export async function removeRemoteServer(
	stateDir: string,
	profileId: string,
): Promise<PublicRemoteConnectionStore> {
	return runStoreOp(async () => {
		const store = await readStoreUnlocked(stateDir);
		store.profiles = store.profiles.filter((p) => p.id !== profileId);
		if (store.active.kind === "remote") {
			if (store.active.profileId === profileId) {
				store.active = { kind: "local" };
			}
		}
		await writeStoreUnlocked(stateDir, store);
		return toPublicStore(store);
	});
}

/** Returns the active remote profile including password, or null in local mode. */
export async function getActiveRemoteProfile(
	stateDir: string,
): Promise<RemoteServerProfile | null> {
	const store = await readRemoteConnectionStore(stateDir);
	if (store.active.kind !== "remote") return null;
	const profileId = store.active.profileId;
	return store.profiles.find((p) => p.id === profileId) ?? null;
}
