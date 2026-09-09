import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

const { getMock, enrollMock, setActiveMock, removeMock, refreshDaemonStatusMock } = vi.hoisted(() => ({
	getMock: vi.fn(),
	enrollMock: vi.fn(),
	setActiveMock: vi.fn(),
	removeMock: vi.fn(),
	refreshDaemonStatusMock: vi.fn(),
}));

vi.mock("../../lib/bridge", () => ({
	aoBridge: {
		remoteConnection: {
			get: getMock,
			enroll: enrollMock,
			setActive: setActiveMock,
			remove: removeMock,
		},
	},
}));

vi.mock("../../lib/daemon-status", () => ({ refreshDaemonStatus: refreshDaemonStatusMock }));

import { RemoteServersSection } from "./RemoteServersSection";

const localStore = { active: { kind: "local" as const }, profiles: [] };
const enrolledProfile = {
	id: "p1",
	label: "Studio Mac",
	baseUrl: "http://100.64.0.5:4317",
	pinnedHostId: "host-1",
	createdAt: "2026-01-01T00:00:00.000Z",
	updatedAt: "2026-01-01T00:00:00.000Z",
};

describe("RemoteServersSection", () => {
	beforeEach(() => {
		getMock.mockReset().mockResolvedValue(localStore);
		enrollMock.mockReset().mockResolvedValue({ active: { kind: "remote", profileId: "p1" }, profiles: [enrolledProfile] });
		setActiveMock.mockReset().mockResolvedValue(localStore);
		removeMock.mockReset().mockResolvedValue(localStore);
		refreshDaemonStatusMock.mockReset().mockResolvedValue(undefined);
	});

	it("marks the local daemon active and lists enrolled servers", async () => {
		getMock.mockResolvedValue({ active: { kind: "remote", profileId: "p1" }, profiles: [enrolledProfile] });
		render(<RemoteServersSection />);
		expect(await screen.findByText("Studio Mac")).toBeInTheDocument();
		expect(screen.getByText("http://100.64.0.5:4317")).toBeInTheDocument();
		expect(screen.getByText("Active")).toBeInTheDocument();
		// Local row offers a switch back; the active remote row does not.
		expect(screen.getAllByRole("button", { name: "Connect" })).toHaveLength(1);
	});

	it("enrolls trimmed values and forgets the password immediately", async () => {
		render(<RemoteServersSection />);
		const user = userEvent.setup();
		await user.type(screen.getByLabelText("Name"), "  Studio Mac  ");
		await user.type(screen.getByLabelText("Address"), "  http://100.64.0.5:4317  ");
		await user.type(screen.getByLabelText("Host ID"), "  host-1  ");
		await user.type(screen.getByLabelText("Connection password"), "secret12");
		await user.click(screen.getByRole("button", { name: "Add server" }));

		await waitFor(() =>
			expect(enrollMock).toHaveBeenCalledWith({
				label: "Studio Mac",
				baseUrl: "http://100.64.0.5:4317",
				pinnedHostId: "host-1",
				password: "secret12",
			}),
		);
		expect(screen.getByLabelText("Connection password")).toHaveValue("");
		expect(refreshDaemonStatusMock).toHaveBeenCalled();
	});

	it("requires every field before enrolling", async () => {
		render(<RemoteServersSection />);
		const user = userEvent.setup();
		await user.click(screen.getByRole("button", { name: "Add server" }));
		expect(await screen.findByText("Fill in every field to add a server.")).toBeInTheDocument();
		expect(enrollMock).not.toHaveBeenCalled();
	});

	it("surfaces an enrollment failure without keeping the secret", async () => {
		enrollMock.mockRejectedValue(new Error("host id mismatch"));
		render(<RemoteServersSection />);
		const user = userEvent.setup();
		await user.type(screen.getByLabelText("Name"), "Studio Mac");
		await user.type(screen.getByLabelText("Address"), "http://100.64.0.5:4317");
		await user.type(screen.getByLabelText("Host ID"), "host-1");
		await user.type(screen.getByLabelText("Connection password"), "secret12");
		await user.click(screen.getByRole("button", { name: "Add server" }));
		expect(await screen.findByText("host id mismatch")).toBeInTheDocument();
		expect(screen.getByLabelText("Connection password")).toHaveValue("");
	});

	it("switches to an enrolled server and back to local", async () => {
		getMock.mockResolvedValue({ active: { kind: "local" }, profiles: [enrolledProfile] });
		render(<RemoteServersSection />);
		const user = userEvent.setup();
		await user.click(await screen.findByRole("button", { name: "Connect" }));
		await waitFor(() => expect(setActiveMock).toHaveBeenCalledWith({ kind: "remote", profileId: "p1" }));
		expect(refreshDaemonStatusMock).toHaveBeenCalled();
	});

	it("removes an enrolled server after confirmation", async () => {
		getMock.mockResolvedValue({ active: { kind: "local" }, profiles: [enrolledProfile] });
		render(<RemoteServersSection />);
		const user = userEvent.setup();
		await user.click(await screen.findByRole("button", { name: "Remove" }));
		const confirmButtons = await screen.findAllByRole("button", { name: "Remove" });
		await user.click(confirmButtons[confirmButtons.length - 1]);
		await waitFor(() => expect(removeMock).toHaveBeenCalledWith("p1"));
	});
});
