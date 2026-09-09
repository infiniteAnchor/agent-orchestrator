package controllers

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/remotewire"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/systeminstall"
)

// Installer is the controller-facing contract for real, async install runs
// against the fixed systeminstall.Target allowlist.
type Installer interface {
	Start(ctx context.Context, target systeminstall.Target) (systeminstall.Job, error)
	StartAgentOperation(ctx context.Context, target systeminstall.Target, method string, operation systeminstall.AgentOperation) (systeminstall.Job, error)
	Status(ctx context.Context, target systeminstall.Target) (systeminstall.Job, error)
	AgentPlans(ctx context.Context) ([]systeminstall.AgentPlan, error)
	AgentJobs(ctx context.Context) ([]systeminstall.Job, error)
	Verify(ctx context.Context, target systeminstall.Target) (systeminstall.Job, error)
}

// SystemInstallController owns the system prerequisite and agent harness install routes.
type SystemInstallController struct {
	Installer Installer
}

// Register mounts the system install routes on the supplied router.
func (c *SystemInstallController) Register(r chi.Router) {
	r.Post("/system/install/{target}", c.start)
	r.Get("/system/install/{target}", c.status)
	r.Get("/agents/installers", c.agentPlans)
	r.Get("/agents/install-jobs", c.agentJobs)
	r.Post("/agents/{agent}/install", c.startAgent)
	r.Get("/agents/{agent}/install", c.agentStatus)
	r.Post("/agents/{agent}/verify", c.verifyAgent)
}

func (c *SystemInstallController) agentPlans(w http.ResponseWriter, r *http.Request) {
	if c.Installer == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/agents/installers")
		return
	}
	plans, err := c.Installer.AgentPlans(r.Context())
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, AgentInstallerCatalogResponse{Agents: agentPlansForWire(r.Context(), plans)})
}

func (c *SystemInstallController) startAgent(w http.ResponseWriter, r *http.Request) {
	if c.Installer == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/agents/{agent}/install")
		return
	}
	target, ok := parseAgentInstallTarget(w, r)
	if !ok {
		return
	}
	var request StartAgentInstallRequest
	if err := decodeJSONStrict(r, &request); err != nil && !errors.Is(err, io.EOF) {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_INSTALL_REQUEST", "invalid install request", nil)
		return
	}
	operation := request.Operation
	if operation == "" {
		operation = systeminstall.AgentOperationInstall
	}
	if operation != systeminstall.AgentOperationInstall && operation != systeminstall.AgentOperationReinstall {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_INSTALL_OPERATION", "operation must be install or reinstall", nil)
		return
	}
	job, err := c.Installer.StartAgentOperation(r.Context(), target, request.Method, operation)
	if err != nil {
		if writeAgentInstallError(w, r, err) {
			return
		}
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusAccepted, installJobForWire(r.Context(), job))
}

func (c *SystemInstallController) agentJobs(w http.ResponseWriter, r *http.Request) {
	if c.Installer == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/agents/install-jobs")
		return
	}
	jobs, err := c.Installer.AgentJobs(r.Context())
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, AgentInstallJobsResponse{Jobs: installJobsForWire(r.Context(), jobs)})
}

func (c *SystemInstallController) verifyAgent(w http.ResponseWriter, r *http.Request) {
	if c.Installer == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/agents/{agent}/verify")
		return
	}
	target, ok := parseAgentInstallTarget(w, r)
	if !ok {
		return
	}
	job, err := c.Installer.Verify(r.Context(), target)
	if err != nil {
		if writeAgentInstallError(w, r, err) {
			return
		}
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusAccepted, installJobForWire(r.Context(), job))
}

// agentPlansForWire removes host-local install destinations and redacts
// absolute paths embedded in the plan's diagnostic copy for LAN requests.
func agentPlansForWire(ctx context.Context, plans []systeminstall.AgentPlan) []systeminstall.AgentPlan {
	if len(plans) == 0 {
		return plans
	}
	out := make([]systeminstall.AgentPlan, len(plans))
	for i, plan := range plans {
		out[i] = agentPlanForWire(ctx, plan)
	}
	return out
}

func agentPlanForWire(ctx context.Context, plan systeminstall.AgentPlan) systeminstall.AgentPlan {
	plan.ExpectedDestination = remotewire.Path(ctx, plan.ExpectedDestination)
	plan.Command = remotewire.Text(ctx, plan.Command)
	plan.Reason = remotewire.Text(ctx, plan.Reason)
	if len(plan.Methods) > 0 {
		methods := make([]systeminstall.AgentInstallMethod, len(plan.Methods))
		for i, method := range plan.Methods {
			method.ExpectedDestination = remotewire.Path(ctx, method.ExpectedDestination)
			method.Command = remotewire.Text(ctx, method.Command)
			method.Reason = remotewire.Text(ctx, method.Reason)
			method.ReinstallCommand = remotewire.Text(ctx, method.ReinstallCommand)
			method.ReinstallReason = remotewire.Text(ctx, method.ReinstallReason)
			methods[i] = method
		}
		plan.Methods = methods
	}
	return plan
}

func installJobsForWire(ctx context.Context, jobs []systeminstall.Job) []systeminstall.Job {
	if len(jobs) == 0 {
		return jobs
	}
	out := make([]systeminstall.Job, len(jobs))
	for i, job := range jobs {
		out[i] = installJobForWire(ctx, job)
	}
	return out
}

// installJobForWire removes the resolved destination and redacts absolute paths
// from installer output and error text for LAN requests. The job's status,
// method, and timestamps stay intact so a remote client can still follow it.
func installJobForWire(ctx context.Context, job systeminstall.Job) systeminstall.Job {
	job.ExpectedDestination = remotewire.Path(ctx, job.ExpectedDestination)
	job.Output = remotewire.Text(ctx, job.Output)
	job.Error = remotewire.Text(ctx, job.Error)
	return job
}

func writeAgentInstallError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case errors.Is(err, systeminstall.ErrInstallMethod):
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INSTALL_METHOD_UNAVAILABLE", "the selected install method is unavailable", nil)
		return true
	case errors.Is(err, systeminstall.ErrHarnessActive):
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict", "HARNESS_ACTIVE", "end active Droid sessions before installing or reinstalling Droid", nil)
		return true
	case errors.Is(err, systeminstall.ErrInstallActive):
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict", "INSTALL_ACTIVE", "an install or verification job is already active for this harness", nil)
		return true
	default:
		return false
	}
}

func (c *SystemInstallController) agentStatus(w http.ResponseWriter, r *http.Request) {
	if c.Installer == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/agents/{agent}/install")
		return
	}
	target, ok := parseAgentInstallTarget(w, r)
	if !ok {
		return
	}
	job, err := c.Installer.Status(r.Context(), target)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, installJobForWire(r.Context(), job))
}

func (c *SystemInstallController) start(w http.ResponseWriter, r *http.Request) {
	if c.Installer == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/system/install/{target}")
		return
	}
	target, ok := parseInstallTarget(w, r)
	if !ok {
		return
	}
	job, err := c.Installer.Start(r.Context(), target)
	if err != nil {
		if writeAgentInstallError(w, r, err) {
			return
		}
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusAccepted, job)
}

func (c *SystemInstallController) status(w http.ResponseWriter, r *http.Request) {
	if c.Installer == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/system/install/{target}")
		return
	}
	target, ok := parseInstallTarget(w, r)
	if !ok {
		return
	}
	job, err := c.Installer.Status(r.Context(), target)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, job)
}

// parseInstallTarget reads and validates the {target} path param against the
// fixed systeminstall allowlist before it ever reaches the service, so a path
// traversal attempt or other junk value gets a clean 400 here rather than
// being passed through.
func parseInstallTarget(w http.ResponseWriter, r *http.Request) (systeminstall.Target, bool) {
	target := systeminstall.Target(chi.URLParam(r, "target"))
	if !systeminstall.IsSystemTarget(target) {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "UNKNOWN_INSTALL_TARGET",
			"unknown install target", nil)
		return "", false
	}
	return target, true
}

func parseAgentInstallTarget(w http.ResponseWriter, r *http.Request) (systeminstall.Target, bool) {
	target := systeminstall.Target(chi.URLParam(r, "agent"))
	if !systeminstall.IsAgentTarget(target) {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "UNKNOWN_AGENT_INSTALL_TARGET",
			"unknown agent install target", nil)
		return "", false
	}
	return target, true
}
