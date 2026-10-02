package controllers

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/taskauto"
)

// TaskAutomationController exposes retry policy, handoffs, and human gates.
type TaskAutomationController struct{ Svc taskauto.Manager }

// Register mounts task automation routes.
func (c *TaskAutomationController) Register(r chi.Router) {
	r.Get("/projects/{id}/task-retry-policy", c.getPolicy)
	r.Put("/projects/{id}/task-retry-policy", c.putPolicy)
	r.Post("/projects/{id}/task-plans/{planId}/tasks/{taskId}/retry", c.retry)
	r.Post("/projects/{id}/task-plans/{planId}/tasks/{taskId}/handoffs", c.recordHandoff)
	r.Get("/projects/{id}/task-plans/{planId}/tasks/{taskId}/handoffs", c.listHandoffs)
	r.Post("/projects/{id}/task-plans/{planId}/tasks/{taskId}/gates", c.openGate)
	r.Get("/projects/{id}/task-plans/{planId}/gates", c.listGates)
	r.Post("/projects/{id}/task-plans/{planId}/gates/{gateId}/approve", c.approve)
	r.Post("/projects/{id}/task-plans/{planId}/gates/{gateId}/reject", c.reject)
}

func (c *TaskAutomationController) getPolicy(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{id}/task-retry-policy")
		return
	}
	policy, err := c.Svc.GetPolicy(r.Context(), projectID(r))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, TaskRetryPolicyEnvelope{Policy: policyForWire(policy)})
}

func (c *TaskAutomationController) putPolicy(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "PUT", "/api/v1/projects/{id}/task-retry-policy")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxTaskPlanBodyBytes)
	var in taskauto.PolicyInput
	if err := decodeTaskPlanJSON(r, &in); err != nil {
		writeTaskProposalDecodeError(w, r, err)
		return
	}
	policy, err := c.Svc.PutPolicy(r.Context(), projectID(r), domain.TaskRetryPolicy{
		MaxAttempts: in.MaxAttempts, FallbackHarness: in.FallbackHarness,
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, TaskRetryPolicyEnvelope{Policy: policyForWire(policy)})
}

func (c *TaskAutomationController) retry(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/projects/{id}/task-plans/{planId}/tasks/{taskId}/retry")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxTaskPlanBodyBytes)
	var in taskauto.RetryInput
	if err := decodeTaskPlanJSON(r, &in); err != nil {
		writeTaskProposalDecodeError(w, r, err)
		return
	}
	outcome, err := c.Svc.Retry(r.Context(), projectID(r), chi.URLParam(r, "planId"), chi.URLParam(r, "taskId"), in)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, TaskRetryDecisionEnvelope{Decision: decisionForWire(outcome)})
}

func (c *TaskAutomationController) recordHandoff(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/projects/{id}/task-plans/{planId}/tasks/{taskId}/handoffs")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxTaskPlanBodyBytes)
	var in taskauto.HandoffInput
	if err := decodeTaskPlanJSON(r, &in); err != nil {
		writeTaskProposalDecodeError(w, r, err)
		return
	}
	handoff, err := c.Svc.RecordHandoff(r.Context(), projectID(r), chi.URLParam(r, "planId"), chi.URLParam(r, "taskId"), in)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusCreated, TaskHandoffEnvelope{Handoff: handoffForWire(handoff)})
}

func (c *TaskAutomationController) listHandoffs(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{id}/task-plans/{planId}/tasks/{taskId}/handoffs")
		return
	}
	rows, err := c.Svc.ListHandoffs(r.Context(), projectID(r), chi.URLParam(r, "planId"), chi.URLParam(r, "taskId"))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	items := make([]TaskHandoffResponse, 0, len(rows))
	for _, row := range rows {
		items = append(items, handoffForWire(row))
	}
	envelope.WriteJSON(w, http.StatusOK, ListTaskHandoffsResponse{Handoffs: items})
}

func (c *TaskAutomationController) openGate(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/projects/{id}/task-plans/{planId}/tasks/{taskId}/gates")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxTaskPlanBodyBytes)
	var in taskauto.GateInput
	if err := decodeTaskPlanJSON(r, &in); err != nil {
		writeTaskProposalDecodeError(w, r, err)
		return
	}
	gate, err := c.Svc.OpenGate(r.Context(), projectID(r), chi.URLParam(r, "planId"), chi.URLParam(r, "taskId"), in)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusCreated, TaskHumanGateEnvelope{Gate: gateForWire(gate)})
}

func (c *TaskAutomationController) listGates(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{id}/task-plans/{planId}/gates")
		return
	}
	rows, err := c.Svc.ListGates(r.Context(), projectID(r), chi.URLParam(r, "planId"))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	items := make([]TaskHumanGateResponse, 0, len(rows))
	for _, row := range rows {
		items = append(items, gateForWire(row))
	}
	envelope.WriteJSON(w, http.StatusOK, ListTaskHumanGatesResponse{Gates: items})
}

func (c *TaskAutomationController) approve(w http.ResponseWriter, r *http.Request) {
	c.resolve(w, r, domain.HumanGateApproved, "POST", "/api/v1/projects/{id}/task-plans/{planId}/gates/{gateId}/approve")
}

func (c *TaskAutomationController) reject(w http.ResponseWriter, r *http.Request) {
	c.resolve(w, r, domain.HumanGateRejected, "POST", "/api/v1/projects/{id}/task-plans/{planId}/gates/{gateId}/reject")
}

func (c *TaskAutomationController) resolve(w http.ResponseWriter, r *http.Request, to domain.HumanGateState, method, path string) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, method, path)
		return
	}
	gate, err := c.Svc.ResolveGate(r.Context(), projectID(r), chi.URLParam(r, "planId"), chi.URLParam(r, "gateId"), to)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, TaskHumanGateEnvelope{Gate: gateForWire(gate)})
}

func policyForWire(policy domain.TaskRetryPolicy) TaskRetryPolicyResponse {
	return TaskRetryPolicyResponse{MaxAttempts: policy.MaxAttempts, FallbackHarness: policy.FallbackHarness}
}

func decisionForWire(outcome domain.RetryOutcome) TaskRetryDecisionResponse {
	out := TaskRetryDecisionResponse{
		ID: outcome.Decision.ID, AttemptID: outcome.Decision.AttemptID,
		Action: string(outcome.Decision.Action), Harness: outcome.Decision.Harness, Reason: outcome.Decision.Reason,
	}
	if outcome.Gate != nil {
		out.GateID = outcome.Gate.ID
	}
	return out
}

func handoffForWire(handoff domain.TaskHandoff) TaskHandoffResponse {
	return TaskHandoffResponse{
		ID: handoff.ID, TaskID: handoff.TaskID, AttemptID: handoff.AttemptID,
		Summary: handoff.Summary, CreatedAt: handoff.CreatedAt,
	}
}

func gateForWire(gate domain.HumanGate) TaskHumanGateResponse {
	return TaskHumanGateResponse{
		ID: gate.ID, PlanID: gate.PlanID, TaskID: gate.TaskID, AttemptID: gate.AttemptID,
		RequestKey: gate.RequestKey, Status: string(gate.State), Summary: gate.Summary,
		CreatedAt: gate.CreatedAt, UpdatedAt: gate.UpdatedAt, ResolvedAt: gate.ResolvedAt,
	}
}
