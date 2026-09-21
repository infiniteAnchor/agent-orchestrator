package controllers_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	taskplansvc "github.com/aoagents/agent-orchestrator/backend/internal/service/taskplan"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func newTaskPlanServer(t *testing.T) *httptest.Server {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	for _, id := range []string{"proj-1", "proj-2"} {
		if err := store.UpsertProject(context.Background(), domain.ProjectRecord{
			ID: id, Path: "/tmp/" + id, RegisteredAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed project %s: %v", id, err)
		}
	}
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	svc := taskplansvc.NewWithDeps(taskplansvc.Deps{
		Store: store,
		Clock: func() time.Time {
			now = now.Add(time.Second)
			return now
		},
	})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{TaskPlans: svc}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTaskPlansAPI_CreateGetListAndScope(t *testing.T) {
	srv := newTaskPlanServer(t)
	request := `{
		"id":"plan-1","title":"Ship it",
		"phases":[{"id":"build","title":"Build"}],
		"tasks":[
			{"id":"implement","phaseId":"build","title":"Implement","prompt":"Write it","dependsOn":[],"verificationCommands":["go test ./..."]},
			{"id":"verify","phaseId":"build","title":"Verify","prompt":"Review it","dependsOn":["implement"],"verificationCommands":[]}
		]
	}`
	body, status, headers := doRequest(t, srv, http.MethodPost, "/api/v1/projects/proj-1/task-plans", request)
	if status != http.StatusCreated {
		t.Fatalf("POST = %d body=%s", status, body)
	}
	assertJSON(t, headers)
	var created struct {
		TaskPlan struct {
			ID        string `json:"id"`
			ProjectID string `json:"projectId"`
		} `json:"taskPlan"`
	}
	mustJSON(t, body, &created)
	if created.TaskPlan.ID != "plan-1" || created.TaskPlan.ProjectID != "proj-1" {
		t.Fatalf("created = %+v", created)
	}

	body, status, _ = doRequest(t, srv, http.MethodGet, "/api/v1/projects/proj-1/task-plans/plan-1", "")
	if status != http.StatusOK {
		t.Fatalf("GET = %d body=%s", status, body)
	}
	var got struct {
		TaskPlan struct {
			ID    string `json:"id"`
			Tasks []struct {
				ID        string   `json:"id"`
				DependsOn []string `json:"dependsOn"`
			} `json:"tasks"`
		} `json:"taskPlan"`
	}
	mustJSON(t, body, &got)
	if got.TaskPlan.ID != "plan-1" || len(got.TaskPlan.Tasks) != 2 || got.TaskPlan.Tasks[1].DependsOn[0] != "implement" {
		t.Fatalf("task plan = %+v", got.TaskPlan)
	}

	body, status, _ = doRequest(t, srv, http.MethodGet, "/api/v1/projects/proj-1/task-plans?limit=1", "")
	if status != http.StatusOK {
		t.Fatalf("LIST = %d body=%s", status, body)
	}
	var list struct {
		TaskPlans []json.RawMessage `json:"taskPlans"`
	}
	mustJSON(t, body, &list)
	if len(list.TaskPlans) != 1 {
		t.Fatalf("list = %s", body)
	}

	body, status, _ = doRequest(t, srv, http.MethodGet, "/api/v1/projects/proj-2/task-plans/plan-1", "")
	assertErrorCode(t, body, status, http.StatusNotFound, "TASK_PLAN_NOT_FOUND")
}

func TestTaskPlansAPI_ValidationConflictAndBounds(t *testing.T) {
	srv := newTaskPlanServer(t)
	valid := `{"id":"plan-1","title":"Plan","phases":[],"tasks":[{"id":"t1","title":"Task","prompt":"Do it","dependsOn":[],"verificationCommands":[]}]}`
	_, status, _ := doRequest(t, srv, http.MethodPost, "/api/v1/projects/proj-1/task-plans", valid)
	if status != http.StatusCreated {
		t.Fatalf("first POST = %d", status)
	}
	body, status, _ := doRequest(t, srv, http.MethodPost, "/api/v1/projects/proj-1/task-plans", valid)
	assertErrorCode(t, body, status, http.StatusConflict, "TASK_PLAN_ALREADY_EXISTS")

	body, status, _ = doRequest(t, srv, http.MethodPost, "/api/v1/projects/proj-1/task-plans", strings.Replace(valid, `"title":"Plan"`, `"title":"Plan","unknown":true`, 1))
	assertErrorCode(t, body, status, http.StatusBadRequest, "INVALID_JSON")

	invalid := `{"id":"bad","title":"Bad","phases":[],"tasks":[{"id":"a","title":"A","prompt":"a","dependsOn":["missing"],"verificationCommands":[]}]}`
	body, status, _ = doRequest(t, srv, http.MethodPost, "/api/v1/projects/proj-1/task-plans", invalid)
	assertErrorCode(t, body, status, http.StatusBadRequest, "INVALID_TASK_PLAN")

	body, status, _ = doRequest(t, srv, http.MethodPost, "/api/v1/projects/proj-1/task-plans", strings.Repeat(" ", (1<<20)+1))
	assertErrorCode(t, body, status, http.StatusRequestEntityTooLarge, "TASK_PLAN_BODY_TOO_LARGE")

	body, status, _ = doRequest(t, srv, http.MethodGet, "/api/v1/projects/proj-1/task-plans?limit=zero", "")
	assertErrorCode(t, body, status, http.StatusBadRequest, "INVALID_QUERY")

	body, status, _ = doRequest(t, srv, http.MethodGet, "/api/v1/projects/proj-1/task-plans?cursor=broken", "")
	assertErrorCode(t, body, status, http.StatusBadRequest, "INVALID_TASK_PLAN_CURSOR")
}

func TestTaskPlansRoutes_DefaultToStubsWithoutService(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/projects/p/task-plans"},
		{http.MethodPost, "/api/v1/projects/p/task-plans"},
		{http.MethodGet, "/api/v1/projects/p/task-plans/x"},
	} {
		body, status, _ := doRequest(t, srv, tc.method, tc.path, `{}`)
		assertErrorCode(t, body, status, http.StatusNotImplemented, "NOT_IMPLEMENTED")
	}
}
