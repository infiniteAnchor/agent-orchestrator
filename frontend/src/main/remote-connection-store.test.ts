import { mkdtemp, readFile, stat } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { describe, expect, it } from "vitest";
import {
	enrollRemoteServer,
	getActiveRemoteProfile,
	readPublicRemoteConnectionStore,
	REMOTE_CONNECTION_FILE_NAME,
	removeRemoteServer,
	setActiveConnectionMode,
} from "./remote-connection-store";

async function tempStateDir(): Promise<string> {
	return mkdtemp(path.join(os.tmpdir(), "ao-remote-conn-"));
}

describe("remote-connection-store", () => {
	it("enrolls a profile, activates it, and never returns the password publicly", async () => {
		const stateDir = await tempStateDir();
		const pub = await enrollRemoteServer(stateDir, {
			label: "Lab",
			baseUrl: "http://100.64.0.2:3011/",
			pinnedHostId: "h_lab",
			password: "secret12",
		});

		expect(pub.active).toEqual({ kind: "remote", profileId: pub.profiles[0]?.id });
		expect(pub.profiles).toHaveLength(1);
		expect(pub.profiles[0]).toMatchObject({
			label: "Lab",
			baseUrl: "http://100.64.0.2:3011",
			pinnedHostId: "h_lab",
		});
		expect(pub.profiles[0] as { password?: string }).not.toHaveProperty("password");

		const active = await getActiveRemoteProfile(stateDir);
		expect(active?.password).toBe("secret12");

		const file = path.join(stateDir, REMOTE_CONNECTION_FILE_NAME);
		const mode = (await stat(file)).mode & 0o777;
		expect(mode).toBe(0o600);
		const onDisk = JSON.parse(await readFile(file, "utf8")) as {
			profiles: Array<{ password?: string }>;
		};
		expect(onDisk.profiles[0]?.password).toBe("secret12");
	});

	it("can switch back to local and remove a profile", async () => {
		const stateDir = await tempStateDir();
		const enrolled = await enrollRemoteServer(stateDir, {
			label: "Box",
			baseUrl: "http://192.168.1.5:3011",
			pinnedHostId: "h_box",
			password: "pw",
		});
		const id = enrolled.profiles[0]!.id;

		const local = await setActiveConnectionMode(stateDir, { kind: "local" });
		expect(local.active).toEqual({ kind: "local" });
		expect(await getActiveRemoteProfile(stateDir)).toBeNull();

		const afterRemove = await removeRemoteServer(stateDir, id);
		expect(afterRemove.profiles).toHaveLength(0);
		expect(await readPublicRemoteConnectionStore(stateDir)).toEqual({
			active: { kind: "local" },
			profiles: [],
		});
	});

	it("rejects an unknown active remote profile id", async () => {
		const stateDir = await tempStateDir();
		await expect(
			setActiveConnectionMode(stateDir, { kind: "remote", profileId: "missing" }),
		).rejects.toThrow(/Unknown remote profile/);
	});
});
