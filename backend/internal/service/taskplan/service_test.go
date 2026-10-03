package taskplan

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

type fakeStore struct {
	project      domain.ProjectRecord
	projectOK    bool
	createErr    error
	created      domain.TaskPlan
	createdAt    time.Time
	plan         domain.TaskPlan
	planOK       bool
	listed       []domain.TaskPlanSummary
	listBefore   time.Time
	listBeforeID string
	listLimit    int
}

func (f *fakeStore) GetProject(context.Context, string) (domain.ProjectRecord, bool, error) {
	return f.project, f.projectOK, nil
}
func (f *fakeStore) CreateTaskPlan(_ context.Context, plan domain.TaskPlan, at time.Time) (domain.TaskPlanSummary, error) {
	f.created, f.createdAt = plan, at
	if f.createErr != nil {
		return domain.TaskPlanSummary{}, f.createErr
	}
	return domain.TaskPlanSummary{ID: plan.ID, ProjectID: plan.ProjectID, Title: plan.Title, CreatedAt: at, UpdatedAt: at}, nil
}
func (f *fakeStore) GetTaskPlan(_ context.Context, _ domain.ProjectID, _ string) (domain.TaskPlan, bool, error) {
	return f.plan, f.planOK, nil
}
func (f *fakeStore) ListTaskPlans(_ context.Context, _ domain.ProjectID, before time.Time, beforeID string, limit int) ([]domain.TaskPlanSummary, error) {
	f.listBefore, f.listBeforeID, f.listLimit = before, beforeID, limit
	return append([]domain.TaskPlanSummary(nil), f.listed...), nil
}

func validInput() CreateInput {
	return CreateInput{
		ID: "plan-1", Title: "Ship it",
		Phases: []PhaseInput{{ID: "build", Title: "Build"}},
		Tasks:  []TaskInput{{ID: "task-1", PhaseID: "build", Title: "Implement", Prompt: "Do the work"}},
	}
}

func activeStore() *fakeStore {
	return &fakeStore{project: domain.ProjectRecord{ID: "proj-1"}, projectOK: true}
}

func TestCreateScopesAndValidatesGraph(t *testing.T) {
	store := activeStore()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.FixedZone("test", -7*60*60))
	svc := NewWithDeps(Deps{Store: store, Clock: func() time.Time { return now }})

	summary, err := svc.Create(context.Background(), "proj-1", validInput())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if store.created.ProjectID != "proj-1" || store.created.ID != "plan-1" || len(store.created.Tasks) != 1 {
		t.Fatalf("created graph = %+v", store.created)
	}
	if !store.createdAt.Equal(now.UTC()) || summary.ProjectID != "proj-1" {
		t.Fatalf("summary/time = %+v / %v", summary, store.createdAt)
	}
}

func TestCreateRejectsInvalidAndOversizedGraphsBeforeWrite(t *testing.T) {
	tests := []struct {
		name string
		edit func(*CreateInput)
		code string
	}{
		{"cycle", func(in *CreateInput) {
			in.Phases = nil
			in.Tasks = []TaskInput{
				{ID: "a", Title: "A", Prompt: "a", DependsOn: []string{"b"}, VerificationCommands: []string{"true"}},
				{ID: "b", Title: "B", Prompt: "b", DependsOn: []string{"a"}, VerificationCommands: []string{"true"}},
			}
		}, "INVALID_TASK_PLAN"},
		{"prompt bound", func(in *CreateInput) { in.Tasks[0].Prompt = strings.Repeat("x", maxTaskPromptBytes+1) }, "TASK_PLAN_TOO_LARGE"},
		{"task count", func(in *CreateInput) { in.Tasks = make([]TaskInput, maxTasks+1) }, "TASK_PLAN_TOO_LARGE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := activeStore()
			in := validInput()
			tt.edit(&in)
			_, err := New(store).Create(context.Background(), "proj-1", in)
			assertAPIErrorCode(t, err, tt.code)
			if store.created.ID != "" {
				t.Fatalf("invalid graph was written: %+v", store.created)
			}
		})
	}
}

func TestCreateReportsTheFieldThatExceededItsOwnBound(t *testing.T) {
	in := validInput()
	in.Tasks[0].Harness = strings.Repeat("h", maxHarnessBytes+1)
	_, err := New(activeStore()).Create(context.Background(), "proj-1", in)
	var api *apierr.Error
	if !errors.As(err, &api) || api.Code != "TASK_PLAN_TOO_LARGE" || api.Details["field"] != "task[0].harness" {
		t.Fatalf("harness bound = %#v, %v", api, err)
	}
	in = validInput()
	in.Tasks[0].WorkspaceKey = strings.Repeat("w", maxWorkspaceKeyBytes+1)
	_, err = New(activeStore()).Create(context.Background(), "proj-1", in)
	if !errors.As(err, &api) || api.Details["field"] != "task[0].workspaceKey" {
		t.Fatalf("workspace bound = %#v, %v", api, err)
	}
}

func TestCreateRejectsMissingArchivedAndDuplicatePlans(t *testing.T) {
	missing := activeStore()
	missing.projectOK = false
	_, err := New(missing).Create(context.Background(), "proj-1", validInput())
	assertAPIErrorCode(t, err, "PROJECT_NOT_FOUND")

	archived := activeStore()
	archived.project.ArchivedAt = time.Now()
	_, err = New(archived).Create(context.Background(), "proj-1", validInput())
	assertAPIErrorCode(t, err, "PROJECT_NOT_FOUND")

	existing := activeStore()
	existing.planOK = true
	_, err = New(existing).Create(context.Background(), "proj-1", validInput())
	assertAPIErrorCode(t, err, "TASK_PLAN_ALREADY_EXISTS")

	raced := activeStore()
	raced.createErr = domain.ErrDuplicateTaskPlan
	_, err = New(raced).Create(context.Background(), "proj-1", validInput())
	assertAPIErrorCode(t, err, "TASK_PLAN_ALREADY_EXISTS")
}

func TestListIsBoundedAndCursorIsStable(t *testing.T) {
	store := activeStore()
	base := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	store.listed = []domain.TaskPlanSummary{
		{ID: "p3", CreatedAt: base.Add(3 * time.Minute)},
		{ID: "p2", CreatedAt: base.Add(2 * time.Minute)},
		{ID: "p1", CreatedAt: base.Add(time.Minute)},
	}
	svc := New(store)
	page, err := svc.List(context.Background(), "proj-1", ListFilter{Limit: 2})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Plans) != 2 || page.NextCursor == "" || store.listLimit != 3 {
		t.Fatalf("page = %+v, store limit = %d", page, store.listLimit)
	}

	store.listed = nil
	if _, err := svc.List(context.Background(), "proj-1", ListFilter{Limit: 2, Cursor: page.NextCursor}); err != nil {
		t.Fatalf("List next page: %v", err)
	}
	if store.listBeforeID != "p2" || !store.listBefore.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("decoded cursor = %q / %v", store.listBeforeID, store.listBefore)
	}
	_, err = svc.List(context.Background(), "proj-1", ListFilter{Cursor: "not-a-cursor"})
	assertAPIErrorCode(t, err, "INVALID_TASK_PLAN_CURSOR")
}

func TestGetReturnsOnlyProjectScopedPlan(t *testing.T) {
	store := activeStore()
	store.plan = domain.TaskPlan{ID: "p1", ProjectID: "proj-1", Title: "Plan", Tasks: []domain.PlannedTask{{ID: "t", Title: "T", Prompt: "P"}}}
	store.planOK = true
	got, err := New(store).Get(context.Background(), "proj-1", "p1")
	if err != nil || got.ID != "p1" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	store.planOK = false
	_, err = New(store).Get(context.Background(), "proj-1", "p1")
	assertAPIErrorCode(t, err, "TASK_PLAN_NOT_FOUND")
}

func assertAPIErrorCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want %s, got nil", want)
	}
	var api *apierr.Error
	if !errors.As(err, &api) || api.Code != want {
		t.Fatalf("error = %v, want API code %s", err, want)
	}
}
