import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { components } from "../../api/schema";
import { TaskPlansWorkspace } from "./TaskPlansDialog";

const mocks = vi.hoisted(() => ({
	create: vi.fn(), accept: vi.fn(), reject: vi.fn(), dispatch: vi.fn(), approve: vi.fn(), rejectGate: vi.fn(),
	proposals: vi.fn(), plans: vi.fn(), plan: vi.fn(), schedule: vi.fn(), gates: vi.fn(),
}));
vi.mock("../hooks/useTaskPlans", () => ({
	useTaskPlanProposals: () => mocks.proposals(), useTaskPlans: () => mocks.plans(),
	useTaskPlan: () => mocks.plan(), useTaskSchedule: () => mocks.schedule(), useTaskHumanGates: () => mocks.gates(),
	useTaskPlanActions: () => ({
		createProposal: { mutateAsync: mocks.create }, acceptProposal: { mutateAsync: mocks.accept },
		rejectProposal: { mutateAsync: mocks.reject }, dispatchPlan: { mutateAsync: mocks.dispatch },
		approveGate: { mutateAsync: mocks.approve }, rejectGate: { mutateAsync: mocks.rejectGate },
	}),
}));
vi.mock("@tanstack/react-router", () => ({ Link: ({ children }: { children: React.ReactNode }) => <a>{children}</a> }));
const graph: components["schemas"]["TaskPlan"] = {
	id: "plan-1", projectId: "project-1", title: "Checkout improvements", phases: [], tasks: [
		{ id: "task-1", title: "Implement checkout", prompt: "Fix totals", dependsOn: [], verificationCommands: ["npm test"], workspaceKey: "checkout", harness: "codex" },
		{ id: "task-2", title: "Review checkout", prompt: "Review tests", dependsOn: ["task-1"], verificationCommands: [], workspaceKey: "review" },
	],
};
const proposal: components["schemas"]["TaskPlanProposal"] = {
	id: "proposal-1", projectId: "project-1", status: "ready", taskPlan: graph, createdAt: "", updatedAt: "",
};
const gate: components["schemas"]["TaskHumanGate"] = {
	id: "gate-1", planId: "plan-1", taskId: "task-1", requestKey: "gate-key", status: "pending", summary: "Review checkout scope", createdAt: "", updatedAt: "",
};
function query<T>(data: T) { return { data, isSuccess: true, isPending: false, isError: false, refetch: vi.fn() }; }
function selectPlan() {
	fireEvent.click(within(screen.getByRole("heading", { name: "Accepted plans" }).parentElement!).getByRole("button", { name: graph.title }));
}
function selectProposal() {
	fireEvent.click(within(screen.getByRole("heading", { name: "Proposals" }).parentElement!).getByRole("button"));
}
beforeEach(() => {
	vi.resetAllMocks();
	mocks.proposals.mockReturnValue(query([proposal]));
	mocks.plans.mockReturnValue(query({ pages: [{ taskPlans: [{ id: "plan-1", title: graph.title }] }] }));
	mocks.plan.mockReturnValue(query(graph));
	mocks.schedule.mockReturnValue(query({ recovery: "complete", readyTaskIds: ["task-1"], tasks: [{ id: "task-1", state: "queued", ready: true }], attempts: [] }));
	mocks.gates.mockReturnValue(query([]));
	mocks.accept.mockResolvedValue({ id: "plan-1" }); mocks.create.mockResolvedValue(proposal);
	mocks.dispatch.mockResolvedValue({ claims: [{ id: "attempt-1" }] });
});
describe("task-plan review", () => {
	it("reviews dependencies and instructions, accepts without dispatching, and dispatches explicitly", async () => {
		render(<TaskPlansWorkspace projectId="project-1" />); selectProposal();
		expect(screen.getByText("Depends on: Implement checkout")).toBeInTheDocument();
		expect(screen.getByText("Fix totals")).toBeInTheDocument();
		expect(screen.getByText("Accepting saves the plan. Tasks start only when you dispatch them; gate approval does not start workers.")).toBeInTheDocument();
		expect(screen.getByText("npm test")).toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "Accept proposal" }));
		await waitFor(() => expect(mocks.accept).toHaveBeenCalledWith("proposal-1"));
		await waitFor(() => expect(screen.getByRole("button", { name: "Dispatch ready tasks" })).toBeEnabled());
		expect(mocks.dispatch).not.toHaveBeenCalled();
		expect(screen.getByText("Start currently ready tasks within server limits. Dispatch again when dependencies finish or gates are approved.")).toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "Dispatch ready tasks" }));
		await waitFor(() => expect(mocks.dispatch).toHaveBeenCalledWith("plan-1"));
		expect(await screen.findByText("Dispatch requested: 1 attempt(s) claimed.")).toBeInTheDocument();
	});
	it("rejects a proposal without dispatching", async () => {
		render(<TaskPlansWorkspace projectId="project-1" />); selectProposal();
		fireEvent.click(screen.getByRole("button", { name: "Reject proposal" }));
		await waitFor(() => expect(mocks.reject).toHaveBeenCalledWith("proposal-1"));
		expect(mocks.dispatch).not.toHaveBeenCalled();
	});
	it.each(["invalid", "failed"])("allows rejecting a %s proposal without offering acceptance", async (status) => {
		mocks.proposals.mockReturnValue(query([{ ...proposal, status }]));
		render(<TaskPlansWorkspace projectId="project-1" />); selectProposal();
		expect(screen.queryByRole("button", { name: "Accept proposal" })).not.toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "Reject proposal" }));
		await waitFor(() => expect(mocks.reject).toHaveBeenCalledWith("proposal-1"));
	});
	it("blocks dispatch during recovery", () => {
		mocks.schedule.mockReturnValue(query({ recovery: "pending", readyTaskIds: ["task-1"], tasks: [], attempts: [] }));
		render(<TaskPlansWorkspace projectId="project-1" />); selectPlan();
		expect(screen.getByRole("button", { name: "Dispatch ready tasks" })).toBeDisabled();
		expect(screen.getByText("Task recovery is in progress. Dispatch is unavailable.")).toBeInTheDocument();
	});
	it("blocks dispatch behind gates and approves without dispatching", async () => {
		mocks.gates.mockReturnValue(query([gate]));
		render(<TaskPlansWorkspace projectId="project-1" />); selectPlan();
		expect(screen.getByRole("button", { name: "Dispatch ready tasks" })).toBeDisabled();
		fireEvent.click(screen.getByRole("button", { name: "Approve gate" }));
		await waitFor(() => expect(mocks.approve).toHaveBeenCalledWith({ planId: "plan-1", gateId: "gate-1" }));
		expect(mocks.dispatch).not.toHaveBeenCalled();
	});
	it("allows dispatch of an independent ready task while another task is gated", async () => {
		mocks.gates.mockReturnValue(query([gate]));
		mocks.plan.mockReturnValue(query({ ...graph, tasks: graph.tasks.map((task) => ({ ...task, dependsOn: [] })) }));
		mocks.schedule.mockReturnValue(query({ recovery: "complete", readyTaskIds: ["task-1", "task-2"], tasks: [], attempts: [] }));
		render(<TaskPlansWorkspace projectId="project-1" />); selectPlan();
		expect(screen.getByRole("button", { name: "Dispatch ready tasks" })).toBeEnabled();
		fireEvent.click(screen.getByRole("button", { name: "Dispatch ready tasks" }));
		await waitFor(() => expect(mocks.dispatch).toHaveBeenCalledWith("plan-1"));
	});
	it("explains cancellation before rejecting a gate", async () => {
		mocks.gates.mockReturnValue(query([gate]));
		render(<TaskPlansWorkspace projectId="project-1" />); selectPlan();
		fireEvent.click(screen.getByRole("button", { name: "Reject gate" }));
		expect(mocks.rejectGate).not.toHaveBeenCalled();
		const confirmation = screen.getByRole("dialog");
		expect(within(confirmation).getByText("Rejecting a gate cancels a task that is still queued.")).toBeInTheDocument();
		fireEvent.click(within(confirmation).getByRole("button", { name: "Reject gate" }));
		await waitFor(() => expect(mocks.rejectGate).toHaveBeenCalledWith({ planId: "plan-1", gateId: "gate-1" }));
	});
	it("bounds specifications by UTF-8 bytes", () => {
		render(<TaskPlansWorkspace projectId="project-1" />);
		fireEvent.change(screen.getByLabelText("Project specification"), { target: { value: "界".repeat(10_923) } });
		expect(screen.getByRole("button", { name: "Generate proposal" })).toBeDisabled();
		fireEvent.change(screen.getByLabelText("Project specification"), { target: { value: "界".repeat(10_922) } });
		expect(screen.getByRole("button", { name: "Generate proposal" })).toBeEnabled();
	});
	it("reuses the request key when retrying a failed submission", async () => {
		mocks.create.mockRejectedValueOnce(new Error("Planner unavailable"));
		render(<TaskPlansWorkspace projectId="project-1" />);
		fireEvent.change(screen.getByLabelText("Project specification"), { target: { value: "Improve checkout" } });
		fireEvent.click(screen.getByRole("button", { name: "Generate proposal" }));
		expect(await screen.findByText("Planner unavailable")).toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "Generate proposal" }));
		await waitFor(() => expect(mocks.create).toHaveBeenCalledTimes(2));
		expect(mocks.create.mock.calls[0][0]).toEqual(mocks.create.mock.calls[1][0]);
		expect(mocks.dispatch).not.toHaveBeenCalled();
	});
	it("surfaces schedule errors, offers retry, and refuses dispatch", () => {
		const refetch = vi.fn();
		mocks.schedule.mockReturnValue({ ...query(undefined), isError: true, error: new Error("Schedule unavailable"), refetch });
		render(<TaskPlansWorkspace projectId="project-1" />); selectPlan();
		expect(screen.getByText("Schedule unavailable")).toBeInTheDocument();
		expect(screen.getByRole("button", { name: "Dispatch ready tasks" })).toBeDisabled();
		fireEvent.click(screen.getByRole("button", { name: "Retry" })); expect(refetch).toHaveBeenCalled();
	});
});
