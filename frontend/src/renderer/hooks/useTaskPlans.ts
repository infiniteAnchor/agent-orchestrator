import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useSyncExternalStore } from "react";
import type { components } from "../../api/schema";
import { apiClient, apiErrorMessage, getApiBaseUrl, hasTrustedApiBaseUrl, subscribeApiBaseUrl } from "../lib/api-client";
import { getDaemonConnectionMode } from "../lib/daemon-connection";

export type TaskPlanProposal = components["schemas"]["TaskPlanProposal"];
export type TaskPlanSummary = components["schemas"]["TaskPlanSummary"];
export type TaskPlan = components["schemas"]["TaskPlan"];
export type TaskSchedule = components["schemas"]["TaskSchedule"];
export type TaskHumanGate = components["schemas"]["TaskHumanGate"];
export type TaskPlanProposalInput = components["schemas"]["CreateTaskPlanProposalInput"];

type DaemonStamp = { mode: ReturnType<typeof getDaemonConnectionMode>; baseUrl: string };
type FencedInput<T> = { input: T; stamp: DaemonStamp };

function assertDaemonStamp(stamp: DaemonStamp) {
	if (!hasTrustedApiBaseUrl() || isPreviewData()) {
		throw new Error("Task plans require a connected AO daemon.");
	}
	if (getDaemonConnectionMode() !== stamp.mode || getApiBaseUrl() !== stamp.baseUrl) {
		throw new Error("The AO daemon changed while this task plan request was in progress.");
	}
}

function useFencedMutation<TInput, TOutput>(options: {
	mutationFn: (input: TInput, stamp: DaemonStamp) => Promise<TOutput>;
	onSuccess?: (output: TOutput, input: TInput, stamp: DaemonStamp) => Promise<unknown> | unknown;
}) {
	const mutation = useMutation<TOutput, Error, FencedInput<TInput>>({
		mutationFn: ({ input, stamp }) => {
			assertDaemonStamp(stamp);
			return options.mutationFn(input, stamp);
		},
		onSuccess: (output, variables) => {
			assertDaemonStamp(variables.stamp);
			return options.onSuccess?.(output, variables.input, variables.stamp);
		},
	});
	return {
		...mutation,
		mutate: (input: TInput) => mutation.mutate({ input, stamp: captureDaemonStamp() }),
		mutateAsync: (input: TInput) => mutation.mutateAsync({ input, stamp: captureDaemonStamp() }),
	};
}

function captureDaemonStamp(): DaemonStamp {
	return { mode: getDaemonConnectionMode(), baseUrl: getApiBaseUrl() };
}

export const taskPlanQueryRoot = ["task-plans"] as const;
export const taskPlanQueryKey = (projectId: string, ...parts: readonly unknown[]) =>
	[...taskPlanQueryRoot, projectId, ...parts] as const;
function isPreviewData() {
	return import.meta.env.VITE_NO_ELECTRON === "1";
}

function useTrustedDaemon() {
	return useSyncExternalStore(subscribeApiBaseUrl, hasTrustedApiBaseUrl, hasTrustedApiBaseUrl);
}

function path(projectId: string): { path: { id: string } };
function path(projectId: string, planId: string): { path: { id: string; planId: string } };
function path(projectId: string, planId?: string): { path: { id: string; planId?: string } } {
	return planId
		? { path: { id: projectId, planId } }
		: { path: { id: projectId } };
}

function requestError(error: unknown): Error {
	return new Error(apiErrorMessage(error));
}

export async function fetchTaskPlanProposals(projectId: string): Promise<TaskPlanProposal[]> {
	const { data, error } = await apiClient.GET("/api/v1/projects/{id}/task-plan-proposals", {
		params: { ...path(projectId), query: { limit: 100 } },
	});
	if (error) throw requestError(error);
	if (!data) throw new Error("The daemon returned no task plan proposals.");
	return data.proposals ?? [];
}

async function fetchPlans(projectId: string, cursor?: string): Promise<components["schemas"]["ListTaskPlansResponse"]> {
	const { data, error } = await apiClient.GET("/api/v1/projects/{id}/task-plans", {
		params: { ...path(projectId), query: { limit: 50, ...(cursor ? { cursor } : {}) } },
	});
	if (error) throw requestError(error);
	return data ?? { taskPlans: [] };
}

async function fetchPlan(projectId: string, planId: string): Promise<TaskPlan | undefined> {
	const { data, error } = await apiClient.GET("/api/v1/projects/{id}/task-plans/{planId}", {
		params: path(projectId, planId),
	});
	if (error) throw requestError(error);
	if (!data?.taskPlan) throw new Error("The daemon returned no task plan.");
	return data.taskPlan;
}

async function fetchSchedule(projectId: string, planId: string): Promise<TaskSchedule | undefined> {
	const { data, error } = await apiClient.GET("/api/v1/projects/{id}/task-plans/{planId}/schedule", {
		params: path(projectId, planId),
	});
	if (error) throw requestError(error);
	if (!data) throw new Error("The daemon returned no task schedule.");
	return data;
}

async function fetchHumanGates(projectId: string, planId: string): Promise<TaskHumanGate[]> {
	const { data, error } = await apiClient.GET("/api/v1/projects/{id}/task-plans/{planId}/gates", {
		params: path(projectId, planId),
	});
	if (error) throw requestError(error);
	if (!data) throw new Error("The daemon returned no task gates.");
	return data.gates ?? [];
}

export function useTaskPlanProposals(projectId: string | undefined) {
	const trusted = useTrustedDaemon();
	return useQuery({
		queryKey: taskPlanQueryKey(projectId ?? "", "proposals"),
		queryFn: () => fetchTaskPlanProposals(projectId!),
		enabled: Boolean(projectId) && trusted,
		// Proposals have no CDC trigger. Refresh only while generation is active.
		refetchInterval: (query) =>
			query.state.data?.some((proposal) => proposal.status === "queued" || proposal.status === "generating")
				? 2_000
				: false,
		retry: 1,
	});
}

export function useTaskPlans(projectId: string | undefined) {
	const trusted = useTrustedDaemon();
	return useInfiniteQuery({
		queryKey: taskPlanQueryKey(projectId ?? "", "list"),
		queryFn: ({ pageParam }) => fetchPlans(projectId!, pageParam || undefined),
		initialPageParam: "",
		getNextPageParam: (lastPage) => lastPage.nextCursor || undefined,
		enabled: Boolean(projectId) && trusted,
		retry: 1,
	});
}

export function useTaskPlan(projectId: string | undefined, planId: string | undefined) {
	const trusted = useTrustedDaemon();
	return useQuery({
		queryKey: taskPlanQueryKey(projectId ?? "", "plan", planId ?? ""),
		queryFn: () => fetchPlan(projectId!, planId!),
		enabled: Boolean(projectId && planId) && trusted,
		retry: 1,
	});
}

export function useTaskSchedule(projectId: string | undefined, planId: string | undefined) {
	const trusted = useTrustedDaemon();
	return useQuery({
		queryKey: taskPlanQueryKey(projectId ?? "", "schedule", planId ?? ""),
		queryFn: () => fetchSchedule(projectId!, planId!),
		enabled: Boolean(projectId && planId) && trusted,
		// The daemon scheduler can claim queued work without a user dispatch call.
		// Poll only while lifecycle work is active; task CDC also invalidates this key.
		refetchInterval: (query) => {
			const schedule = query.state.data;
			if (schedule?.recovery !== "complete") return 2_000;
			return schedule.tasks.some((task) =>
				["queued", "claimed", "running", "collecting"].includes(task.state),
			) ? 2_000 : false;
		},
		retry: 1,
	});
}

export function useTaskHumanGates(projectId: string | undefined, planId: string | undefined) {
	const trusted = useTrustedDaemon();
	return useQuery({
		queryKey: taskPlanQueryKey(projectId ?? "", "gates", planId ?? ""),
		queryFn: () => fetchHumanGates(projectId!, planId!),
		enabled: Boolean(projectId && planId) && trusted,
		// Gate rows do not emit CDC events. Keep an open gate view current while it is mounted.
		refetchInterval: 5_000,
		retry: 1,
	});
}

export type CreateTaskPlanProposalInput = TaskPlanProposalInput;
export type TaskGateMutationInput = { planId: string; gateId: string };

export function useTaskPlanActions(projectId: string) {
	const queryClient = useQueryClient();
	const invalidateProject = () => queryClient.invalidateQueries({ queryKey: taskPlanQueryKey(projectId) });
	const createProposal = useFencedMutation({
		mutationFn: async (input: CreateTaskPlanProposalInput, stamp) => {
			const { data, error } = await apiClient.POST("/api/v1/projects/{id}/task-plan-proposals", {
				params: path(projectId), body: input,
			});
			assertDaemonStamp(stamp);
			if (error) throw requestError(error);
			if (!data?.proposal) throw new Error("The daemon did not return a task plan proposal.");
			return data.proposal;
		},
		onSuccess: async (proposal) => {
			queryClient.setQueryData<TaskPlanProposal[]>(taskPlanQueryKey(projectId, "proposals"), (current) => {
				const existing = current ?? [];
				return [proposal, ...existing.filter((item) => item.id !== proposal.id)].slice(0, 100);
			});
			await invalidateProject();
		},
	});
	const acceptProposal = useFencedMutation({
		mutationFn: async (proposalId: string, stamp) => {
			const { data, error } = await apiClient.POST("/api/v1/projects/{id}/task-plan-proposals/{proposalId}/accept", {
				params: { path: { id: projectId, proposalId } },
			});
			assertDaemonStamp(stamp);
			if (error) throw requestError(error);
			if (!data?.taskPlan) throw new Error("The daemon did not return the accepted task plan.");
			return data.taskPlan;
		},
		onSuccess: () => invalidateProject(),
	});
	const rejectProposal = useFencedMutation({
		mutationFn: async (proposalId: string, stamp) => {
			const { data, error } = await apiClient.POST("/api/v1/projects/{id}/task-plan-proposals/{proposalId}/reject", {
				params: { path: { id: projectId, proposalId } },
			});
			assertDaemonStamp(stamp);
			if (error) throw requestError(error);
			if (!data?.proposal) throw new Error("The daemon did not return the rejected proposal.");
			return data.proposal;
		},
		onSuccess: () => invalidateProject(),
	});
	const dispatchPlan = useFencedMutation({
		mutationFn: async (planId: string, stamp) => {
			const { data, error } = await apiClient.POST("/api/v1/projects/{id}/task-plans/{planId}/dispatch", {
				params: path(projectId, planId),
			});
			assertDaemonStamp(stamp);
			if (error) throw requestError(error);
			if (!data) throw new Error("The daemon did not return dispatch results.");
			return data;
		},
		onSuccess: async (_data, planId) => {
			await Promise.all([
				invalidateProject(),
				queryClient.invalidateQueries({ queryKey: taskPlanQueryKey(projectId, "plan", planId) }),
				queryClient.invalidateQueries({ queryKey: taskPlanQueryKey(projectId, "schedule", planId) }),
				queryClient.invalidateQueries({ queryKey: taskPlanQueryKey(projectId, "gates", planId) }),
			]);
		},
	});
	const useGateMutation = (decision: "approve" | "reject") => useFencedMutation({
		mutationFn: async ({ planId, gateId }: TaskGateMutationInput, stamp) => {
			const params = { params: { path: { id: projectId, planId, gateId } } };
			if (decision === "approve") {
				const { error } = await apiClient.POST("/api/v1/projects/{id}/task-plans/{planId}/gates/{gateId}/approve", params);
				assertDaemonStamp(stamp);
				if (error) throw requestError(error);
			} else {
				const { error } = await apiClient.POST("/api/v1/projects/{id}/task-plans/{planId}/gates/{gateId}/reject", params);
				assertDaemonStamp(stamp);
				if (error) throw requestError(error);
			}
		},
		onSuccess: (_data, { planId }) => {
			return Promise.all([
				queryClient.invalidateQueries({ queryKey: taskPlanQueryKey(projectId, "plan", planId) }),
				queryClient.invalidateQueries({ queryKey: taskPlanQueryKey(projectId, "schedule", planId) }),
				queryClient.invalidateQueries({ queryKey: taskPlanQueryKey(projectId, "gates", planId) }),
			]);
		},
	});
	return {
		createProposal,
		acceptProposal,
		rejectProposal,
		dispatchPlan,
		approveGate: useGateMutation("approve"),
		rejectGate: useGateMutation("reject"),
	};
}
