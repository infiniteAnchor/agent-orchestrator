import { useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "@tanstack/react-router";
import { ListTodo, RefreshCw } from "lucide-react";
import type { components } from "../../api/schema";
import type { MessageKey } from "../i18n/messages";
import { apiErrorMessage } from "../lib/api-client";
import { useShellMaybe } from "../lib/shell-context";
import {
	useTaskPlan, useTaskPlanActions, useTaskPlanProposals,
	useTaskPlans, useTaskSchedule, useTaskHumanGates,
} from "../hooks/useTaskPlans";
import { TopbarButton } from "./TopbarButton";
import { Button } from "./ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle, DialogTrigger } from "./ui/dialog";
import { ConfirmDialog } from "./ConfirmDialog";

type TaskPlan = components["schemas"]["TaskPlan"];
type Selection = { kind: "proposal" | "plan"; id: string };

const statusKeys: Record<string, MessageKey> = {
	queued: "taskPlans.statusQueued", generating: "taskPlans.statusGenerating",
	ready: "taskPlans.statusReady", invalid: "taskPlans.statusInvalid",
	failed: "taskPlans.statusFailed", accepted: "taskPlans.statusAccepted",
	rejected: "taskPlans.statusRejected", pending: "taskPlans.statusPending",
	approved: "taskPlans.statusApproved", running: "taskPlans.statusRunning",
	claimed: "taskPlans.statusClaimed", launching: "taskPlans.statusLaunching",
	collecting: "taskPlans.statusVerifying", succeeded: "taskPlans.statusSucceeded",
	completed: "taskPlans.statusCompleted", blocked: "taskPlans.statusBlocked",
	cancelled: "taskPlans.statusCancelled", held: "taskPlans.statusHeld",
	dispatching: "taskPlans.statusDispatching",
};

function Status({ value }: { value: string }) {
	const { t } = useTranslation();
	return <span className="rounded border border-border px-2 py-0.5 text-xs text-muted-foreground">{t(statusKeys[value] ?? "taskPlans.statusUnknown")}</span>;
}

export function TaskPlansButton({ projectId }: { projectId: string }) {
	const { t } = useTranslation();
	const [open, setOpen] = useState(false);
	const shell = useShellMaybe();
	const connectionId = shell?.daemonStatus.remote?.profileId ?? "local";
	return (
		<Dialog open={open} onOpenChange={setOpen}>
			<DialogTrigger asChild>
				<TopbarButton variant="accent" className="topbar-control--labeled" aria-label={t("taskPlans.title")}>
					<ListTodo className="size-icon-md" aria-hidden="true" />
					<span data-compact-label>{t("taskPlans.title")}</span>
				</TopbarButton>
			</DialogTrigger>
			{open ? <DialogContent className="flex h-[min(800px,90svh)] w-[min(1100px,95vw)] max-w-none flex-col overflow-hidden">
				<DialogHeader>
					<DialogTitle>{t("taskPlans.title")}</DialogTitle>
					<DialogDescription>{t("taskPlans.description")}</DialogDescription>
				</DialogHeader>
				<TaskPlansWorkspace key={`${connectionId}:${projectId}`} projectId={projectId} />
			</DialogContent> : null}
		</Dialog>
	);
}

function QueryError({ error, retry }: { error: unknown; retry: () => void }) {
	const { t } = useTranslation();
	return <div role="alert" className="space-y-2 text-sm text-destructive">
		<p>{apiErrorMessage(error, t("taskPlans.loadFailed"))}</p>
		<Button variant="outline" size="sm" onClick={retry}>{t("taskPlans.retry")}</Button>
	</div>;
}

export function TaskPlansWorkspace({ projectId }: { projectId: string }) {
	const { t } = useTranslation();
	const proposals = useTaskPlanProposals(projectId);
	const plans = useTaskPlans(projectId);
	const actions = useTaskPlanActions(projectId);
	const [specification, setSpecification] = useState("");
	const requestKey = useRef<string | null>(null);
	const [selection, setSelection] = useState<Selection | null>(null);
	const [actionError, setActionError] = useState<string | null>(null);
	const [notice, setNotice] = useState<string | null>(null);
	const [rejectGateId, setRejectGateId] = useState<string | null>(null);
	const proposal = proposals.data?.find((item) => selection?.kind === "proposal" && item.id === selection.id);
	const planId = selection?.kind === "plan" ? selection.id : proposal?.taskPlanId;
	const planQuery = useTaskPlan(projectId, planId);
	const schedule = useTaskSchedule(projectId, planId);
	const gates = useTaskHumanGates(projectId, planId);
	const graph = planId ? planQuery.data : proposal?.taskPlan;
	const summaries = plans.data?.pages.flatMap((page) => page.taskPlans) ?? [];
	const busy = Object.values(actions).some((mutation) => mutation.isPending);
	const tooLarge = new TextEncoder().encode(specification).length > 32_768;
	const gatedTaskIds = new Set(gates.data?.filter((gate) => gate.status === "pending").map((gate) => gate.taskId));
	const canDispatch = planQuery.data !== undefined && !planQuery.isError && schedule.data?.recovery === "complete" && !schedule.isError && !gates.isError && gates.data !== undefined &&
		schedule.data.readyTaskIds.some((id) => !gatedTaskIds.has(id));

	async function run(action: () => Promise<void>) {
		setActionError(null);
		setNotice(null);
		try { await action(); }
		catch (error) { setActionError(apiErrorMessage(error, t("taskPlans.actionFailed"))); }
	}

	function select(next: Selection) {
		setSelection(next);
		setActionError(null);
		setNotice(null);
		setRejectGateId(null);
	}

	const refresh = () => {
		void proposals.refetch();
		void plans.refetch();
		if (planId) {
			void planQuery.refetch();
			void schedule.refetch();
			void gates.refetch();
		}
	};

	return <div className="flex min-h-0 flex-1 flex-col gap-4">
		<form className="space-y-2" onSubmit={(event) => {
			event.preventDefault();
			if (busy || tooLarge || !specification.trim()) return;
			void run(async () => {
				requestKey.current ??= crypto.randomUUID();
				const created = await actions.createProposal.mutateAsync({ specification, requestKey: requestKey.current });
				setSelection({ kind: "proposal", id: created.id });
				setSpecification("");
				requestKey.current = null;
			});
		}}>
			<label htmlFor="task-plan-specification" className="text-sm font-medium">{t("taskPlans.specification")}</label>
			<textarea id="task-plan-specification" className="block min-h-20 w-full resize-y rounded-md border border-border bg-background p-2 text-sm" value={specification} disabled={busy} onChange={(event) => {
				setSpecification(event.target.value);
				requestKey.current = null;
			}} aria-describedby="task-plan-specification-help" />
			<div className="flex flex-wrap items-center justify-between gap-2">
				<p id="task-plan-specification-help" className="text-xs text-muted-foreground">{tooLarge ? t("taskPlans.tooLarge") : t("taskPlans.plannerHint")}</p>
				<Button type="submit" disabled={busy || tooLarge || !specification.trim()}>{actions.createProposal.isPending ? t("taskPlans.generating") : t("taskPlans.generate")}</Button>
			</div>
		</form>
		{actionError ? <p role="alert" className="text-sm text-destructive">{actionError}</p> : null}
		{notice ? <p role="status" className="text-sm text-muted-foreground">{notice}</p> : null}
		<div className="grid min-h-0 flex-1 grid-cols-1 gap-4 overflow-y-auto sm:grid-cols-[240px_minmax(0,1fr)] sm:overflow-hidden">
			<aside className="space-y-4 sm:overflow-y-auto">
				<Button variant="outline" size="sm" onClick={refresh}><RefreshCw className="size-3.5" aria-hidden="true" />{t("taskPlans.refresh")}</Button>
				<section className="space-y-2">
					<h3 className="text-sm font-semibold">{t("taskPlans.proposals")}</h3>
					{proposals.isPending ? <p>{t("taskPlans.loading")}</p> : proposals.isError ? <QueryError error={proposals.error} retry={() => void proposals.refetch()} /> : null}
					{proposals.data?.length === 0 ? <p className="text-xs text-muted-foreground">{t("taskPlans.proposalsEmpty")}</p> : null}
					{proposals.data?.map((item) => <button type="button" key={item.id} disabled={busy} aria-pressed={selection?.kind === "proposal" && selection.id === item.id} onClick={() => select({ kind: "proposal", id: item.id })} className="flex w-full flex-col items-start gap-2 rounded-md border border-border p-2 text-left text-sm hover:bg-muted aria-pressed:bg-muted">
						<span className="max-w-full break-words">{item.taskPlan?.title || item.id}</span>
						<Status value={item.status} />
					</button>)}
					{proposals.data?.length === 100 ? <p className="text-xs text-muted-foreground">{t("taskPlans.proposalLimit")}</p> : null}
				</section>
				<section className="space-y-2">
					<h3 className="text-sm font-semibold">{t("taskPlans.plans")}</h3>
					{plans.isPending ? <p>{t("taskPlans.loading")}</p> : plans.isError ? <QueryError error={plans.error} retry={() => void plans.refetch()} /> : null}
					{plans.isSuccess && summaries.length === 0 ? <p className="text-xs text-muted-foreground">{t("taskPlans.plansEmpty")}</p> : null}
					{summaries.map((item) => <button type="button" key={item.id} disabled={busy} aria-pressed={selection?.kind === "plan" && selection.id === item.id} onClick={() => select({ kind: "plan", id: item.id })} className="block w-full break-words rounded-md border border-border p-2 text-left text-sm hover:bg-muted aria-pressed:bg-muted">{item.title}</button>)}
					{plans.hasNextPage ? <Button variant="outline" size="sm" disabled={plans.isFetchingNextPage} onClick={() => void plans.fetchNextPage()}>{t("taskPlans.loadMore")}</Button> : null}
				</section>
			</aside>
			<main className="min-w-0 space-y-4 sm:overflow-y-auto">
				{!selection ? <p className="text-sm text-muted-foreground">{t("taskPlans.select")}</p> : null}
				{proposal ? <section className="space-y-3">
					<Status value={proposal.status} />
					{proposal.errorMessage ? <p role="alert" className="whitespace-pre-wrap break-words text-sm text-destructive">{proposal.errorMessage}</p> : null}
					{proposal.status === "queued" || proposal.status === "generating" ? <p role="status">{t("taskPlans.generating")}</p> : null}
					{proposal.status === "ready" ? <>
						<p className="text-xs text-muted-foreground">{t("taskPlans.acceptanceHint")}</p>
						<div className="flex gap-2">
							<Button disabled={busy || !proposal.taskPlan || proposals.isError} onClick={() => void run(async () => {
								const accepted = await actions.acceptProposal.mutateAsync(proposal.id);
								setSelection({ kind: "plan", id: accepted.id });
							})}>{t("taskPlans.accept")}</Button>
							<Button variant="outline" disabled={busy} onClick={() => void run(async () => { await actions.rejectProposal.mutateAsync(proposal.id); })}>{t("taskPlans.reject")}</Button>
						</div>
					</> : null}
					{proposal.status === "invalid" || proposal.status === "failed" ? <Button variant="outline" disabled={busy || proposals.isError} onClick={() => void run(async () => { await actions.rejectProposal.mutateAsync(proposal.id); })}>{t("taskPlans.reject")}</Button> : null}
				</section> : null}
				{planId ? <>
					{planQuery.isPending ? <p>{t("taskPlans.loading")}</p> : planQuery.isError ? <QueryError error={planQuery.error} retry={() => void planQuery.refetch()} /> : null}
					<section className="space-y-2">
						<Button disabled={busy || !canDispatch} onClick={() => void run(async () => {
							const dispatched = await actions.dispatchPlan.mutateAsync(planId);
							setNotice(t("taskPlans.dispatched", { count: dispatched.claims.length }));
						})}>{t("taskPlans.dispatch")}</Button>
						<p className="text-xs text-muted-foreground">{t("taskPlans.dispatchHint")}</p>
						{schedule.data?.recovery !== undefined && schedule.data.recovery !== "complete" ? <p role="status">{t("taskPlans.recovery")}</p> : null}
						{schedule.isPending ? <p>{t("taskPlans.loading")}</p> : schedule.isError ? <QueryError error={schedule.error} retry={() => void schedule.refetch()} /> : null}
					</section>
					<section className="space-y-2">
						<h3 className="text-sm font-semibold">{t("taskPlans.gates")}</h3>
						{gates.isPending ? <p>{t("taskPlans.loading")}</p> : gates.isError ? <QueryError error={gates.error} retry={() => void gates.refetch()} /> : null}
						{gates.data?.length === 0 ? <p className="text-xs text-muted-foreground">{t("taskPlans.gatesEmpty")}</p> : null}
						{gates.data?.map((gate) => <article key={gate.id} className="space-y-2 rounded-md border border-border p-3">
							<div className="flex flex-wrap items-center gap-2"><span className="break-words text-sm">{graph?.tasks.find((task) => task.id === gate.taskId)?.title ?? gate.taskId}</span><Status value={gate.status} /></div>
							<p className="whitespace-pre-wrap break-words text-sm">{gate.summary}</p>
							{gate.status === "pending" ? <div className="flex gap-2">
								<Button size="sm" disabled={busy || gates.isError} onClick={() => void run(async () => { await actions.approveGate.mutateAsync({ planId, gateId: gate.id }); })}>{t("taskPlans.approveGate")}</Button>
								<Button size="sm" variant="outline" disabled={busy || gates.isError} onClick={() => setRejectGateId(gate.id)}>{t("taskPlans.rejectGate")}</Button>
							</div> : null}
						</article>)}
					</section>
				</> : null}
				{graph ? <TaskGraph graph={graph} schedule={schedule.isError ? undefined : schedule.data} gatedTaskIds={gatedTaskIds} /> : null}
			</main>
		</div>
		<ConfirmDialog open={rejectGateId !== null} title={t("taskPlans.rejectGate")} description={t("taskPlans.rejectGateHint")} confirmLabel={t("taskPlans.rejectGate")} destructive busy={busy} error={actionError} onOpenChange={(next) => { if (!next) setRejectGateId(null); }} onConfirm={() => {
			if (!planId || !rejectGateId || busy) return;
			void run(async () => {
				await actions.rejectGate.mutateAsync({ planId, gateId: rejectGateId });
				setRejectGateId(null);
			});
		}} />
	</div>;
}

function TaskGraph({ graph, schedule, gatedTaskIds }: { graph: TaskPlan; schedule?: components["schemas"]["TaskSchedule"]; gatedTaskIds: Set<string> }) {
	const { t } = useTranslation();
	return <section className="space-y-3">
		<h2 className="break-words text-lg font-semibold">{graph.title}</h2>
		{graph.tasks.map((task) => {
			const state = schedule?.tasks.find((item) => item.id === task.id);
			const attempts = schedule?.attempts.filter((item) => item.taskId === task.id) ?? [];
			const phase = graph.phases.find((item) => item.id === task.phaseId);
			return <article key={task.id} className="space-y-3 rounded-md border border-border p-3">
				<div className="flex flex-wrap items-center gap-2"><h3 className="break-words font-medium">{task.title}</h3>{state ? <Status value={state.state} /> : null}{state?.ready && !gatedTaskIds.has(task.id) ? <span className="text-xs text-muted-foreground">{t("taskPlans.ready")}</span> : null}</div>
				{phase ? <p className="text-xs text-muted-foreground">{t("taskPlans.phase")}: {phase.title}</p> : null}
				<p className="break-words text-xs text-muted-foreground">{task.dependsOn.length ? `${t("taskPlans.dependencies")}: ${task.dependsOn.map((id) => graph.tasks.find((item) => item.id === id)?.title ?? id).join(", ")}` : t("taskPlans.independent")}</p>
				<p className="break-words text-xs text-muted-foreground">{t("taskPlans.workspace")}: {task.workspaceKey}{task.harness ? ` · ${t("taskPlans.harness")}: ${task.harness}` : ""}</p>
				<details><summary className="cursor-pointer text-sm">{t("taskPlans.prompt")}</summary><p className="mt-2 whitespace-pre-wrap break-words text-sm">{task.prompt}</p></details>
				{task.verificationCommands.length ? <details><summary className="cursor-pointer text-sm">{t("taskPlans.verification")}</summary><pre className="mt-2 whitespace-pre-wrap break-words text-xs">{task.verificationCommands.join("\n")}</pre></details> : null}
				{attempts.length ? <div className="space-y-2"><h4 className="text-xs font-medium">{t("taskPlans.attempts")}</h4>{attempts.map((attempt) => <div key={attempt.id} className="flex flex-wrap items-center gap-2 text-xs"><span>{attempt.attemptNumber}</span><Status value={attempt.state} />{attempt.sessionId ? <Link className="text-primary underline" to="/projects/$projectId/sessions/$sessionId" params={{ projectId: graph.projectId, sessionId: attempt.sessionId }}>{t("taskPlans.openSession")}</Link> : null}</div>)}</div> : null}
			</article>;
		})}
	</section>;
}
