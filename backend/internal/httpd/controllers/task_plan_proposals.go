package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	taskplansvc "github.com/aoagents/agent-orchestrator/backend/internal/service/taskplan"
)

// TaskPlanProposalsController exposes authenticated project-scoped planner drafts.
type TaskPlanProposalsController struct{ Svc taskplansvc.ProposalManager }

// Register mounts task plan proposal routes.
func (c *TaskPlanProposalsController) Register(r chi.Router) {
	r.Get("/projects/{id}/task-plan-proposals", c.list)
	r.Post("/projects/{id}/task-plan-proposals", c.create)
	r.Get("/projects/{id}/task-plan-proposals/{proposalId}", c.get)
	r.Post("/projects/{id}/task-plan-proposals/{proposalId}/accept", c.accept)
	r.Post("/projects/{id}/task-plan-proposals/{proposalId}/reject", c.reject)
}

func (c *TaskPlanProposalsController) create(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/projects/{id}/task-plan-proposals")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxTaskPlanBodyBytes)
	var in taskplansvc.CreateProposalInput
	if err := decodeTaskPlanJSON(r, &in); err != nil {
		writeTaskProposalDecodeError(w, r, err)
		return
	}
	proposal, err := c.Svc.Create(r.Context(), projectID(r), in)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusAccepted, TaskPlanProposalEnvelope{Proposal: taskPlanProposalForWire(r.Context(), proposal)})
}

func (c *TaskPlanProposalsController) get(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{id}/task-plan-proposals/{proposalId}")
		return
	}
	proposal, err := c.Svc.Get(r.Context(), projectID(r), chi.URLParam(r, "proposalId"))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, TaskPlanProposalEnvelope{Proposal: taskPlanProposalForWire(r.Context(), proposal)})
}

func (c *TaskPlanProposalsController) list(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{id}/task-plan-proposals")
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_QUERY", "limit must be a positive integer", nil)
			return
		}
		limit = parsed
	}
	proposals, err := c.Svc.List(r.Context(), projectID(r), limit)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	items := make([]TaskPlanProposalResponse, 0, len(proposals))
	for _, proposal := range proposals {
		items = append(items, taskPlanProposalForWire(r.Context(), proposal))
	}
	envelope.WriteJSON(w, http.StatusOK, ListTaskPlanProposalsResponse{Proposals: items})
}

func (c *TaskPlanProposalsController) accept(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/projects/{id}/task-plan-proposals/{proposalId}/accept")
		return
	}
	summary, err := c.Svc.Accept(r.Context(), projectID(r), chi.URLParam(r, "proposalId"))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusCreated, TaskPlanSummaryEnvelope{TaskPlan: taskPlanSummaryForWire(r.Context(), summary)})
}

func (c *TaskPlanProposalsController) reject(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/projects/{id}/task-plan-proposals/{proposalId}/reject")
		return
	}
	proposal, err := c.Svc.Reject(r.Context(), projectID(r), chi.URLParam(r, "proposalId"))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, TaskPlanProposalEnvelope{Proposal: taskPlanProposalForWire(r.Context(), proposal)})
}

func writeTaskProposalDecodeError(w http.ResponseWriter, r *http.Request, err error) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		envelope.WriteAPIError(w, r, http.StatusRequestEntityTooLarge, "bad_request", "TASK_PROPOSAL_BODY_TOO_LARGE", "Task proposal body exceeds 1 MiB", nil)
		return
	}
	envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
}

func taskPlanProposalForWire(ctx context.Context, proposal domain.TaskPlanProposal) TaskPlanProposalResponse {
	out := TaskPlanProposalResponse{
		ID: proposal.ID, ProjectID: proposal.ProjectID, Status: string(proposal.State),
		ErrorCode: proposal.ErrorCode, ErrorMessage: proposal.ErrorMessage,
		CreatedAt: proposal.CreatedAt, UpdatedAt: proposal.UpdatedAt,
	}
	if proposal.State == domain.TaskPlanProposalAccepted {
		out.TaskPlanID = proposal.ID
	}
	if proposal.GraphJSON != "" {
		var plan domain.TaskPlan
		if json.Unmarshal([]byte(proposal.GraphJSON), &plan) == nil {
			wire := taskPlanForWire(ctx, plan)
			out.TaskPlan = &wire
		}
	}
	return out
}
