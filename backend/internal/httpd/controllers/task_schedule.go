package controllers

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/tasksched"
)

func (c *TaskPlansController) schedule(w http.ResponseWriter, r *http.Request) {
	if c == nil || c.Schedule == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{id}/task-plans/{planId}/schedule")
		return
	}
	snap, err := c.Schedule.Snapshot(r.Context(), projectID(r), chi.URLParam(r, "planId"))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, scheduleResponse(snap))
}

func (c *TaskPlansController) dispatch(w http.ResponseWriter, r *http.Request) {
	if c == nil || c.Schedule == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/projects/{id}/task-plans/{planId}/dispatch")
		return
	}
	report, err := c.Schedule.Dispatch(r.Context(), projectID(r), chi.URLParam(r, "planId"))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	claims := make([]TaskScheduleAttempt, 0, len(report.Claims))
	for _, claim := range report.Claims {
		claims = append(claims, attemptResponse(claim))
	}
	envelope.WriteJSON(w, http.StatusOK, TaskDispatchResponse{Recovery: report.Recovery, Claims: claims})
}

func (c *TaskPlansController) candidate(w http.ResponseWriter, r *http.Request) {
	if c == nil || c.Schedule == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/projects/{id}/task-plans/{planId}/tasks/{taskId}/candidate")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var in tasksched.Candidate
	if err := decodeTaskPlanJSON(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	done, err := c.Schedule.SubmitCandidate(r.Context(), projectID(r), chi.URLParam(r, "planId"), chi.URLParam(r, "taskId"), in)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, TaskCandidateResponse{
		AttemptID: done.AttemptID, TaskID: done.TaskID, Outcome: string(done.Outcome), TaskState: string(done.TaskState),
	})
}

func scheduleResponse(snap tasksched.Snapshot) TaskScheduleResponse {
	tasks := make([]TaskScheduleTask, 0, len(snap.Tasks))
	for _, task := range snap.Tasks {
		tasks = append(tasks, TaskScheduleTask{
			ID: task.ID, State: string(task.State), WorkspaceKey: task.WorkspaceKey, Harness: task.Harness, Ready: task.Ready,
		})
	}
	attempts := make([]TaskScheduleAttempt, 0, len(snap.Attempts))
	for _, attempt := range snap.Attempts {
		attempts = append(attempts, attemptResponse(attempt))
	}
	ready := snap.ReadyTaskIDs
	if ready == nil {
		ready = []string{}
	}
	return TaskScheduleResponse{Recovery: snap.Recovery, ReadyTaskIDs: ready, Tasks: tasks, Attempts: attempts}
}

func attemptResponse(attempt tasksched.AttemptStatus) TaskScheduleAttempt {
	return TaskScheduleAttempt{
		ID: attempt.ID, TaskID: attempt.TaskID, AttemptNumber: attempt.AttemptNumber,
		State: string(attempt.State), RuntimeRef: attempt.RuntimeRef, SessionID: attempt.SessionID,
	}
}
