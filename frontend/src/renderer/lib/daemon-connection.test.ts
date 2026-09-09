import { beforeEach, describe, expect, it, vi } from "vitest";
import {
	getDaemonConnectionMode,
	getRemoteDaemonBaseUrl,
	isRemoteDaemon,
	setDaemonConnection,
	subscribeDaemonConnection,
} from "./daemon-connection";

describe("daemon connection store", () => {
	beforeEach(() => {
		setDaemonConnection({ kind: "local" }, null);
	});

	it("defaults to the local daemon", () => {
		expect(isRemoteDaemon()).toBe(false);
		expect(getDaemonConnectionMode()).toEqual({ kind: "local" });
		expect(getRemoteDaemonBaseUrl()).toBeNull();
	});

	it("records the remote profile and its public base URL", () => {
		setDaemonConnection({ kind: "remote", profileId: "p1" }, "http://100.64.0.5:4317");
		expect(isRemoteDaemon()).toBe(true);
		expect(getDaemonConnectionMode()).toEqual({ kind: "remote", profileId: "p1" });
		expect(getRemoteDaemonBaseUrl()).toBe("http://100.64.0.5:4317");
	});

	it("notifies subscribers only on a real change", () => {
		const listener = vi.fn();
		const unsubscribe = subscribeDaemonConnection(listener);

		setDaemonConnection({ kind: "remote", profileId: "p1" }, "http://a:1");
		expect(listener).toHaveBeenCalledTimes(1);

		// Same profile and URL: no spurious rebind of long-lived transports.
		setDaemonConnection({ kind: "remote", profileId: "p1" }, "http://a:1");
		expect(listener).toHaveBeenCalledTimes(1);

		setDaemonConnection({ kind: "remote", profileId: "p2" }, "http://b:2");
		expect(listener).toHaveBeenCalledTimes(2);

		setDaemonConnection({ kind: "local" }, null);
		expect(listener).toHaveBeenCalledTimes(3);
		expect(getRemoteDaemonBaseUrl()).toBeNull();

		unsubscribe();
		setDaemonConnection({ kind: "remote", profileId: "p3" }, "http://c:3");
		expect(listener).toHaveBeenCalledTimes(3);
	});

	it("ignores a base URL supplied in local mode", () => {
		setDaemonConnection({ kind: "local" }, "http://leftover:1");
		expect(getRemoteDaemonBaseUrl()).toBeNull();
	});
});
