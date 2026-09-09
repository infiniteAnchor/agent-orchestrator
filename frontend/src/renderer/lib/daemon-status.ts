import { aoBridge } from "./bridge";
import { setApiBaseUrl, setApiDaemonStatus } from "./api-client";
import { setDaemonConnection } from "./daemon-connection";

export type DaemonStatus = Awaited<ReturnType<typeof aoBridge.daemon.getStatus>>;

export function applyDaemonStatus(nextStatus: DaemonStatus): void {
	setApiDaemonStatus(nextStatus);
	if (nextStatus.state === "ready" && nextStatus.remote) {
		// Remote mode: main owns the bearer and proxies every transport. The base
		// URL is recorded for display and for rebinding long-lived connections.
		setDaemonConnection(
			{ kind: "remote", profileId: nextStatus.remote.profileId },
			nextStatus.remote.baseUrl,
		);
		setApiBaseUrl(nextStatus.remote.baseUrl);
		return;
	}
	setDaemonConnection({ kind: "local" }, null);
	if (nextStatus.state === "ready" && nextStatus.port) {
		setApiBaseUrl(`http://127.0.0.1:${nextStatus.port}`);
	} else {
		setApiBaseUrl(null);
	}
}

export async function refreshDaemonStatus(): Promise<DaemonStatus> {
	const nextStatus = await readDaemonStatus();
	applyDaemonStatus(nextStatus);
	return nextStatus;
}

export function readDaemonStatus(): Promise<DaemonStatus> {
	return aoBridge.daemon.getStatus();
}
