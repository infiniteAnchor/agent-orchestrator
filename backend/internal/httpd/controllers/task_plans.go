package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/remotewire"
	taskplansvc "github.com/aoagents/agent-orchestrator/backend/internal/service/taskplan"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/tasksched"
)

const maxTaskPlanBodyBytes = 1 << 20

// TaskPlansController owns the bounded project-scoped task-plan surface.
type TaskPlansController struct {
	Svc      taskplansvc.Manager
	Schedule tasksched.API
}

// Register mounts the project-scoped task-plan routes.
func (c *TaskPlansController) Register(r chi.Router) {
	r.Get("/projects/{id}/task-plans", c.list)
	r.Post("/projects/{id}/task-plans", c.create)
	r.Get("/projects/{id}/task-plans/{planId}", c.get)
	r.Get("/projects/{id}/task-plans/{planId}/schedule", c.schedule)
	r.Post("/projects/{id}/task-plans/{planId}/dispatch", c.dispatch)
	r.Post("/projects/{id}/task-plans/{planId}/tasks/{taskId}/candidate", c.candidate)
}

func (c *TaskPlansController) create(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/projects/{id}/task-plans")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxTaskPlanBodyBytes)
	var in taskplansvc.CreateInput
	if err := decodeTaskPlanJSON(r, &in); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			envelope.WriteAPIError(w, r, http.StatusRequestEntityTooLarge, "bad_request", "TASK_PLAN_BODY_TOO_LARGE", "Task plan body exceeds 1 MiB", nil)
			return
		}
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	summary, err := c.Svc.Create(r.Context(), projectID(r), in)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusCreated, TaskPlanSummaryEnvelope{TaskPlan: taskPlanSummaryForWire(r.Context(), summary)})
}

func (c *TaskPlansController) get(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{id}/task-plans/{planId}")
		return
	}
	plan, err := c.Svc.Get(r.Context(), projectID(r), chi.URLParam(r, "planId"))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, TaskPlanEnvelope{TaskPlan: taskPlanForWire(r.Context(), plan)})
}

func (c *TaskPlansController) list(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{id}/task-plans")
		return
	}
	filter, err := parseTaskPlanListFilter(r)
	if err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_QUERY", err.Error(), nil)
		return
	}
	page, err := c.Svc.List(r.Context(), projectID(r), filter)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	items := make([]TaskPlanSummaryResponse, 0, len(page.Plans))
	for _, summary := range page.Plans {
		items = append(items, taskPlanSummaryForWire(r.Context(), summary))
	}
	envelope.WriteJSON(w, http.StatusOK, ListTaskPlansResponse{TaskPlans: items, NextCursor: page.NextCursor})
}

func decodeTaskPlanJSON(r *http.Request, out any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func parseTaskPlanListFilter(r *http.Request) (taskplansvc.ListFilter, error) {
	limit := taskplansvc.DefaultListLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			return taskplansvc.ListFilter{}, errors.New("limit must be a positive integer")
		}
		limit = parsed
	}
	if limit > taskplansvc.MaxListLimit {
		limit = taskplansvc.MaxListLimit
	}
	return taskplansvc.ListFilter{Limit: limit, Cursor: r.URL.Query().Get("cursor")}, nil
}

func taskPlanSummaryForWire(ctx context.Context, summary domain.TaskPlanSummary) TaskPlanSummaryResponse {
	return TaskPlanSummaryResponse{
		ID: summary.ID, ProjectID: summary.ProjectID, Title: remotewire.Text(ctx, summary.Title),
		CreatedAt: summary.CreatedAt, UpdatedAt: summary.UpdatedAt,
	}
}

func taskPlanForWire(ctx context.Context, plan domain.TaskPlan) TaskPlanResponse {
	phases := make([]TaskPlanPhaseResponse, 0, len(plan.Phases))
	for _, phase := range plan.Phases {
		phases = append(phases, TaskPlanPhaseResponse{ID: phase.ID, Title: remotewire.Text(ctx, phase.Title)})
	}
	tasks := make([]TaskPlanTaskResponse, 0, len(plan.Tasks))
	for _, task := range plan.Tasks {
		dependencies := append([]string{}, task.DependsOn...)
		commands := make([]string, 0, len(task.VerificationCommands))
		for _, command := range task.VerificationCommands {
			commands = append(commands, remotewire.Text(ctx, command))
		}
		tasks = append(tasks, TaskPlanTaskResponse{
			ID: task.ID, PhaseID: task.PhaseID, Title: remotewire.Text(ctx, task.Title),
			Prompt: remotewire.Text(ctx, task.Prompt), DependsOn: dependencies,
			VerificationCommands: commands,
			WorkspaceKey:         remotewire.Text(ctx, task.WorkspaceKey),
			Harness:              task.Harness,
		})
	}
	return TaskPlanResponse{
		ID: plan.ID, ProjectID: plan.ProjectID, Title: remotewire.Text(ctx, plan.Title),
		Phases: phases, Tasks: tasks,
	}
}
