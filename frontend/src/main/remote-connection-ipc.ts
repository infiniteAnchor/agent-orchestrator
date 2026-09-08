// IPC for remote AO server enrollment / mode selection (Phase 2).
// Public payloads never include the connection password.

import { ipcMain } from "electron";
import type {
	ConnectionMode,
	EnrollRemoteServerInput,
} from "../shared/remote-connection";
import {
	enrollRemoteServer,
	readPublicRemoteConnectionStore,
	removeRemoteServer,
	setActiveConnectionMode,
	type PublicRemoteConnectionStore,
} from "./remote-connection-store";
import { verifyPinnedIdentity } from "./remote-identity-gate";

export const REMOTE_CONNECTION_GET_CHANNEL = "remoteConnection:get";
export const REMOTE_CONNECTION_ENROLL_CHANNEL = "remoteConnection:enroll";
export const REMOTE_CONNECTION_SET_ACTIVE_CHANNEL = "remoteConnection:setActive";
export const REMOTE_CONNECTION_REMOVE_CHANNEL = "remoteConnection:remove";
export const REMOTE_CONNECTION_VERIFY_CHANNEL = "remoteConnection:verifyIdentity";

export function installRemoteConnectionIPC(getStateDir: () => string): void {
	ipcMain.handle(
		REMOTE_CONNECTION_GET_CHANNEL,
		async (): Promise<PublicRemoteConnectionStore> => readPublicRemoteConnectionStore(getStateDir()),
	);

	ipcMain.handle(
		REMOTE_CONNECTION_ENROLL_CHANNEL,
		async (_event, input: unknown): Promise<PublicRemoteConnectionStore> => {
			if (typeof input !== "object" || input === null) {
				throw new Error("Enrollment payload must be an object.");
			}
			const body = input as Partial<EnrollRemoteServerInput>;
			if (
				typeof body.label !== "string" ||
				typeof body.baseUrl !== "string" ||
				typeof body.pinnedHostId !== "string" ||
				typeof body.password !== "string"
			) {
				throw new Error("Enrollment requires label, baseUrl, pinnedHostId, and password.");
			}
			// Verify identity before storing the secret so a wrong host never
			// receives the password on a later proxied call from a bad pin.
			const gate = await verifyPinnedIdentity({
				baseUrl: body.baseUrl,
				pinnedHostId: body.pinnedHostId,
			});
			if (!gate.ok) {
				throw new Error(gate.message);
			}
			return enrollRemoteServer(getStateDir(), {
				label: body.label,
				baseUrl: body.baseUrl,
				pinnedHostId: body.pinnedHostId,
				password: body.password,
			});
		},
	);

	ipcMain.handle(
		REMOTE_CONNECTION_SET_ACTIVE_CHANNEL,
		async (_event, mode: unknown): Promise<PublicRemoteConnectionStore> => {
			if (typeof mode !== "object" || mode === null) {
				throw new Error("Active mode must be an object.");
			}
			const body = mode as Partial<ConnectionMode>;
			if (body.kind === "local") {
				return setActiveConnectionMode(getStateDir(), { kind: "local" });
			}
			if (body.kind === "remote" && typeof body.profileId === "string") {
				return setActiveConnectionMode(getStateDir(), {
					kind: "remote",
					profileId: body.profileId,
				});
			}
			throw new Error("Active mode must be { kind: 'local' } or { kind: 'remote', profileId }.");
		},
	);

	ipcMain.handle(
		REMOTE_CONNECTION_REMOVE_CHANNEL,
		async (_event, profileId: unknown): Promise<PublicRemoteConnectionStore> => {
			if (typeof profileId !== "string" || profileId === "") {
				throw new Error("profileId must be a non-empty string.");
			}
			return removeRemoteServer(getStateDir(), profileId);
		},
	);

	ipcMain.handle(REMOTE_CONNECTION_VERIFY_CHANNEL, async () => {
		const { getActiveRemoteProfile } = await import("./remote-connection-store");
		const active = await getActiveRemoteProfile(getStateDir());
		if (active === null) {
			return { ok: false as const, reason: "no_remote_profile" as const, message: "No remote server is selected." };
		}
		return verifyPinnedIdentity({
			baseUrl: active.baseUrl,
			pinnedHostId: active.pinnedHostId,
		});
	});
}
