import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook, waitFor, act } from "@testing-library/react";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const { getMock, postMock, trustedMock, subscribeMock, baseUrlMock } = vi.hoisted(() => ({
	getMock: vi.fn(),
	postMock: vi.fn(),
	trustedMock: vi.fn(() => true),
	subscribeMock: vi.fn(() => () => {}),
	baseUrlMock: vi.fn(() => "http://127.0.0.1:3001"),
}));

vi.mock("../lib/api-client", () => ({
	apiClient: { GET: getMock, POST: postMock },
	apiErrorMessage: (error: unknown) =>
		typeof error === "object" && error !== null && "message" in error
			? String((error as { message: unknown }).message)
			: "Request failed",
	hasTrustedApiBaseUrl: trustedMock,
	subscribeApiBaseUrl: subscribeMock,
	getApiBaseUrl: baseUrlMock,
}));

import {
	taskPlanQueryKey,
	fetchTaskPlanProposals,
	useTaskPlanActions,
	useTaskPlans,
} from "./useTaskPlans";
import { setDaemonConnection } from "../lib/daemon-connection";

function wrapper(queryClient: QueryClient) {
	return ({ children }: { children: ReactNode }) => (
		<QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
	);
}

function makeClient() {
	return new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
}

beforeEach(() => {
	getMock.mockReset();
	postMock.mockReset();
	trustedMock.mockReset().mockReturnValue(true);
	baseUrlMock.mockReset().mockReturnValue("http://127.0.0.1:3001");
	setDaemonConnection({ kind: "local" }, null);
	vi.stubEnv("VITE_NO_ELECTRON", "0");
});

describe("task plan data hooks", () => {
	it("loads cursor pages under the project-scoped list key", async () => {
		getMock
			.mockResolvedValueOnce({ data: { taskPlans: [{ id: "p1" }], nextCursor: "next" } })
			.mockResolvedValueOnce({ data: { taskPlans: [{ id: "p2" }] } });
		const client = makeClient();
		const { result } = renderHook(() => useTaskPlans("project-a"), { wrapper: wrapper(client) });

		await waitFor(() => expect(result.current.data?.pages).toHaveLength(1));
		await act(() => result.current.fetchNextPage());

		expect(getMock).toHaveBeenNthCalledWith(1, "/api/v1/projects/{id}/task-plans", {
			params: { path: { id: "project-a" }, query: { limit: 50 } },
		});
		expect(getMock).toHaveBeenNthCalledWith(2, "/api/v1/projects/{id}/task-plans", {
			params: { path: { id: "project-a" }, query: { limit: 50, cursor: "next" } },
		});
		expect(client.getQueryCache().find({ queryKey: taskPlanQueryKey("project-a", "list") })).toBeDefined();
	});

	it("turns API error envelopes into readable errors", async () => {
		getMock.mockResolvedValue({ error: { message: "Project was archived" } });
		await expect(fetchTaskPlanProposals("project-a")).rejects.toThrow("Project was archived");
	});

	it("keeps acceptance separate from dispatch and invalidates only this project", async () => {
		postMock.mockResolvedValue({ data: { taskPlan: { id: "plan-1", projectId: "project-a" } } });
		const client = makeClient();
		const invalidate = vi.spyOn(client, "invalidateQueries").mockResolvedValue(undefined);
		const { result } = renderHook(() => useTaskPlanActions("project-a"), { wrapper: wrapper(client) });

		await act(() => result.current.acceptProposal.mutateAsync("proposal-1"));

		expect(postMock).toHaveBeenCalledOnce();
		expect(postMock).toHaveBeenCalledWith(
			"/api/v1/projects/{id}/task-plan-proposals/{proposalId}/accept",
			{ params: { path: { id: "project-a", proposalId: "proposal-1" } } },
		);
		expect(postMock.mock.calls.some(([url]) => String(url).endsWith("/dispatch"))).toBe(false);
		expect(invalidate).toHaveBeenCalledWith({ queryKey: taskPlanQueryKey("project-a") });
		expect(invalidate).not.toHaveBeenCalledWith({ queryKey: taskPlanQueryKey("project-b") });
	});

	it("puts a created proposal into the project cache before the list refetch", async () => {
		const proposal = { id: "proposal-1", projectId: "project-a", status: "queued" };
		postMock.mockResolvedValue({ data: { proposal } });
		const client = makeClient();
		const { result } = renderHook(() => useTaskPlanActions("project-a"), { wrapper: wrapper(client) });

		await act(() => result.current.createProposal.mutateAsync({ requestKey: "r1", specification: "build it" }));

		expect(client.getQueryData(taskPlanQueryKey("project-a", "proposals"))).toEqual([proposal]);
	});

	it("drops a late mutation result after switching daemon profiles", async () => {
		let resolvePost!: (value: unknown) => void;
		postMock.mockReturnValue(new Promise((resolve) => { resolvePost = resolve; }));
		const client = makeClient();
		const { result } = renderHook(() => useTaskPlanActions("project-a"), { wrapper: wrapper(client) });
		await act(async () => {
			const pending = result.current.createProposal.mutateAsync({ requestKey: "r1", specification: "build it" });
			await waitFor(() => expect(postMock).toHaveBeenCalledOnce());
			setDaemonConnection({ kind: "remote", profileId: "second-server" }, "https://second-server.example");
			baseUrlMock.mockReturnValue("https://second-server.example");
			client.clear();
			resolvePost({ data: { proposal: { id: "from-first-server", projectId: "project-a", status: "queued" } } });
			await expect(pending).rejects.toThrow("The AO daemon changed while this task plan request was in progress.");
		});
		expect(client.getQueryData(taskPlanQueryKey("project-a", "proposals"))).toBeUndefined();
	});

	it("uses the explicit dispatch route only when dispatchPlan is called", async () => {
		postMock.mockResolvedValue({ data: {} });
		const { result } = renderHook(() => useTaskPlanActions("project-a"), { wrapper: wrapper(makeClient()) });

		await act(() => result.current.dispatchPlan.mutateAsync("plan-1"));

		expect(postMock).toHaveBeenCalledWith("/api/v1/projects/{id}/task-plans/{planId}/dispatch", {
			params: { path: { id: "project-a", planId: "plan-1" } },
		});
	});

	it("does not call the API when no trusted daemon is available", async () => {
		trustedMock.mockReturnValue(false);
		const { result } = renderHook(() => useTaskPlanActions("project-a"), { wrapper: wrapper(makeClient()) });

		await expect(result.current.createProposal.mutateAsync({ requestKey: "r1", specification: "spec" }))
			.rejects.toThrow("Task plans require a connected AO daemon.");
		expect(postMock).not.toHaveBeenCalled();
	});
});
