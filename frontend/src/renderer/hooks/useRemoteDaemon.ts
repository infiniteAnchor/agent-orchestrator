import { useSyncExternalStore } from "react";
import { isRemoteDaemon, subscribeDaemonConnection } from "../lib/daemon-connection";

/**
 * Whether this desktop is currently driving an enrolled remote AO server.
 * Re-renders the component when the user switches servers, so loopback-only
 * surfaces can disappear and come back without an app restart.
 */
export function useIsRemoteDaemon(): boolean {
	return useSyncExternalStore(subscribeDaemonConnection, isRemoteDaemon, isRemoteDaemon);
}
