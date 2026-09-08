/** Active desktop→daemon connection mode. */
export type ConnectionMode =
	| { kind: "local" }
	| { kind: "remote"; profileId: string };

/** Public profile facts safe to show in the renderer (no connection secret). */
export type RemoteServerProfilePublic = {
	id: string;
	label: string;
	baseUrl: string;
	pinnedHostId: string;
	createdAt: string;
	updatedAt: string;
};

/** Enrollment input collected once in the UI and handed to main over IPC. */
export type EnrollRemoteServerInput = {
	label: string;
	baseUrl: string;
	pinnedHostId: string;
	/** Connection password from `ao lan enable|regenerate`. Main stores it; renderer must forget it. */
	password: string;
};
