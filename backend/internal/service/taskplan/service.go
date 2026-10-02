// Package taskplan exposes project-scoped durable task-plan use cases.
// Scheduling, dispatch, attempts, and result collection deliberately remain
// outside this package until the later Phase 3 slices own those transitions.
package taskplan

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

const (
	// DefaultListLimit is the default number of plan summaries in one page.
	DefaultListLimit = 50
	// MaxListLimit is the largest plan-summary page the service permits.
	MaxListLimit = 100

	maxPlanIDBytes       = 128
	maxPlanTitleBytes    = 512
	maxPhases            = 100
	maxTasks             = 500
	maxPhaseIDBytes      = 128
	maxPhaseTitleBytes   = 512
	maxTaskIDBytes       = 128
	maxTaskTitleBytes    = 512
	maxTaskPromptBytes   = 64 * 1024
	maxDependencyEdges   = 5_000
	maxVerificationSteps = 1_000
	maxCommandBytes      = 8 * 1024
	maxWorkspaceKeyBytes = 128
	maxHarnessBytes      = 64
)

// Store is the narrow durable surface required by task-plan use cases.
type Store interface {
	GetProject(context.Context, string) (domain.ProjectRecord, bool, error)
	CreateTaskPlan(context.Context, domain.TaskPlan, time.Time) (domain.TaskPlanSummary, error)
	GetTaskPlan(context.Context, domain.ProjectID, string) (domain.TaskPlan, bool, error)
	ListTaskPlans(context.Context, domain.ProjectID, time.Time, string, int) ([]domain.TaskPlanSummary, error)
}

// Manager is the controller-facing task-plan contract.
type Manager interface {
	Create(context.Context, domain.ProjectID, CreateInput) (domain.TaskPlanSummary, error)
	Get(context.Context, domain.ProjectID, string) (domain.TaskPlan, error)
	List(context.Context, domain.ProjectID, ListFilter) (ListPage, error)
}

// PhaseInput is optional presentation metadata in a task-plan create request.
type PhaseInput struct {
	ID    string `json:"id" maxLength:"128"`
	Title string `json:"title" maxLength:"512"`
}

// TaskInput is one proposed task and its explicit dependency edges.
type TaskInput struct {
	ID                   string   `json:"id" maxLength:"128"`
	PhaseID              string   `json:"phaseId,omitempty" maxLength:"128"`
	Title                string   `json:"title" maxLength:"512"`
	Prompt               string   `json:"prompt" maxLength:"65536"`
	DependsOn            []string `json:"dependsOn,omitempty" maxItems:"5000"`
	VerificationCommands []string `json:"verificationCommands,omitempty" maxItems:"1000"`
	WorkspaceKey         string   `json:"workspaceKey,omitempty" maxLength:"128"`
	Harness              string   `json:"harness,omitempty" maxLength:"64"`
}

// CreateInput omits projectId because ownership comes exclusively from the
// route. A body cannot redirect a graph into a different project.
type CreateInput struct {
	ID     string       `json:"id" maxLength:"128"`
	Title  string       `json:"title" maxLength:"512"`
	Phases []PhaseInput `json:"phases,omitempty" maxItems:"100"`
	Tasks  []TaskInput  `json:"tasks" minItems:"1" maxItems:"500"`
}

// ListFilter selects one bounded newest-first summary page.
type ListFilter struct {
	Limit  int
	Cursor string
}

// ListPage is one bounded page plus an opaque continuation cursor.
type ListPage struct {
	Plans      []domain.TaskPlanSummary
	NextCursor string
}

// Service implements project-scoped task-plan creation and reads.
type Service struct {
	store Store
	now   func() time.Time
}

// Deps configures the task-plan service and its testable clock.
type Deps struct {
	Store Store
	Clock func() time.Time
}

// New constructs a task-plan service with the system clock.
func New(store Store) *Service { return NewWithDeps(Deps{Store: store}) }

// NewWithDeps constructs a task-plan service with explicit dependencies.
func NewWithDeps(d Deps) *Service {
	now := d.Clock
	if now == nil {
		now = time.Now
	}
	return &Service{store: d.Store, now: now}
}

var _ Manager = (*Service)(nil)

// Create validates and atomically persists a graph for an active project.
func (s *Service) Create(ctx context.Context, projectID domain.ProjectID, in CreateInput) (domain.TaskPlanSummary, error) {
	if err := s.requireProject(ctx, projectID); err != nil {
		return domain.TaskPlanSummary{}, err
	}
	plan, err := graphFromInput(projectID, in)
	if err != nil {
		return domain.TaskPlanSummary{}, err
	}
	if _, ok, err := s.store.GetTaskPlan(ctx, projectID, plan.ID); err != nil {
		return domain.TaskPlanSummary{}, apierr.Internal("TASK_PLAN_LOAD_FAILED", "Failed to check task plan identity")
	} else if ok {
		return domain.TaskPlanSummary{}, duplicatePlan()
	}
	summary, err := s.store.CreateTaskPlan(ctx, plan, s.now().UTC())
	if errors.Is(err, domain.ErrDuplicateTaskPlan) {
		return domain.TaskPlanSummary{}, duplicatePlan()
	}
	if errors.Is(err, domain.ErrTaskPlanProjectNotFound) {
		return domain.TaskPlanSummary{}, apierr.NotFound("PROJECT_NOT_FOUND", "Unknown project")
	}
	if err != nil {
		return domain.TaskPlanSummary{}, apierr.Internal("TASK_PLAN_CREATE_FAILED", "Failed to create task plan")
	}
	return summary, nil
}

// Get returns one full graph scoped to its active owning project.
func (s *Service) Get(ctx context.Context, projectID domain.ProjectID, planID string) (domain.TaskPlan, error) {
	if err := s.requireProject(ctx, projectID); err != nil {
		return domain.TaskPlan{}, err
	}
	if err := validateID("task plan id", planID, maxPlanIDBytes); err != nil {
		return domain.TaskPlan{}, apierr.Invalid("INVALID_TASK_PLAN_ID", err.Error(), nil)
	}
	plan, ok, err := s.store.GetTaskPlan(ctx, projectID, planID)
	if err != nil {
		return domain.TaskPlan{}, apierr.Internal("TASK_PLAN_LOAD_FAILED", "Failed to load task plan")
	}
	if !ok {
		return domain.TaskPlan{}, apierr.NotFound("TASK_PLAN_NOT_FOUND", "Unknown task plan")
	}
	return plan, nil
}

// List returns one stable newest-first page of plan summaries.
func (s *Service) List(ctx context.Context, projectID domain.ProjectID, filter ListFilter) (ListPage, error) {
	if err := s.requireProject(ctx, projectID); err != nil {
		return ListPage{}, err
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}
	beforeCreatedAt, beforeID, err := decodeCursor(filter.Cursor)
	if err != nil {
		return ListPage{}, err
	}
	rows, err := s.store.ListTaskPlans(ctx, projectID, beforeCreatedAt, beforeID, limit+1)
	if err != nil {
		return ListPage{}, apierr.Internal("TASK_PLANS_LIST_FAILED", "Failed to load task plans")
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	page := ListPage{Plans: rows}
	if hasMore {
		page.NextCursor = encodeCursor(rows[len(rows)-1])
	}
	return page, nil
}

func (s *Service) requireProject(ctx context.Context, projectID domain.ProjectID) error {
	if s == nil || s.store == nil {
		return apierr.Internal("TASK_PLAN_SERVICE_UNAVAILABLE", "Task plan service is unavailable")
	}
	record, ok, err := s.store.GetProject(ctx, string(projectID))
	if err != nil {
		return apierr.Internal("PROJECT_LOAD_FAILED", "Failed to load project")
	}
	if !ok || !record.ArchivedAt.IsZero() {
		return apierr.NotFound("PROJECT_NOT_FOUND", "Unknown project")
	}
	return nil
}

func graphFromInput(projectID domain.ProjectID, in CreateInput) (domain.TaskPlan, error) {
	if err := validateInputBounds(in); err != nil {
		return domain.TaskPlan{}, err
	}
	plan := domain.TaskPlan{ID: in.ID, ProjectID: string(projectID), Title: in.Title}
	plan.Phases = make([]domain.TaskPhase, 0, len(in.Phases))
	for _, phase := range in.Phases {
		plan.Phases = append(plan.Phases, domain.TaskPhase{ID: phase.ID, Title: phase.Title})
	}
	plan.Tasks = make([]domain.PlannedTask, 0, len(in.Tasks))
	for _, task := range in.Tasks {
		plan.Tasks = append(plan.Tasks, domain.PlannedTask{
			ID: task.ID, PhaseID: task.PhaseID, Title: task.Title, Prompt: task.Prompt,
			DependsOn:            append([]string(nil), task.DependsOn...),
			VerificationCommands: append([]string(nil), task.VerificationCommands...),
			WorkspaceKey:         task.WorkspaceKey,
			Harness:              task.Harness,
		})
	}
	if err := plan.Validate(); err != nil {
		return domain.TaskPlan{}, apierr.Invalid("INVALID_TASK_PLAN", err.Error(), nil)
	}
	return plan, nil
}

func validateInputBounds(in CreateInput) error {
	checks := []struct {
		name  string
		value string
		limit int
	}{{"task plan id", in.ID, maxPlanIDBytes}, {"task plan title", in.Title, maxPlanTitleBytes}}
	for _, check := range checks {
		if len(check.value) > check.limit {
			return tooLarge(check.name, check.limit)
		}
	}
	if len(in.Phases) > maxPhases {
		return tooMany("phases", maxPhases)
	}
	if len(in.Tasks) > maxTasks {
		return tooMany("tasks", maxTasks)
	}
	edges, commands := 0, 0
	for i, phase := range in.Phases {
		if len(phase.ID) > maxPhaseIDBytes {
			return tooLarge(fmt.Sprintf("phase[%d].id", i), maxPhaseIDBytes)
		}
		if len(phase.Title) > maxPhaseTitleBytes {
			return tooLarge(fmt.Sprintf("phase[%d].title", i), maxPhaseTitleBytes)
		}
	}
	for i, task := range in.Tasks {
		if len(task.ID) > maxTaskIDBytes {
			return tooLarge(fmt.Sprintf("task[%d].id", i), maxTaskIDBytes)
		}
		if len(task.PhaseID) > maxPhaseIDBytes {
			return tooLarge(fmt.Sprintf("task[%d].phaseId", i), maxPhaseIDBytes)
		}
		if len(task.Title) > maxTaskTitleBytes {
			return tooLarge(fmt.Sprintf("task[%d].title", i), maxTaskTitleBytes)
		}
		if len(task.Prompt) > maxTaskPromptBytes {
			return tooLarge(fmt.Sprintf("task[%d].prompt", i), maxTaskPromptBytes)
		}
		edges += len(task.DependsOn)
		commands += len(task.VerificationCommands)
		for j, dependency := range task.DependsOn {
			if len(dependency) > maxTaskIDBytes {
				return tooLarge(fmt.Sprintf("task[%d].dependsOn[%d]", i, j), maxTaskIDBytes)
			}
		}
		for j, command := range task.VerificationCommands {
			if len(command) > maxCommandBytes {
				return tooLarge(fmt.Sprintf("task[%d].verificationCommands[%d]", i, j), maxCommandBytes)
			}
		}
		if len(task.WorkspaceKey) > maxWorkspaceKeyBytes {
			return tooLarge(fmt.Sprintf("task[%d].workspaceKey", i), maxWorkspaceKeyBytes)
		}
		if len(task.Harness) > maxHarnessBytes {
			return tooLarge(fmt.Sprintf("task[%d].harness", i), maxHarnessBytes)
		}
	}
	if edges > maxDependencyEdges {
		return tooMany("dependency edges", maxDependencyEdges)
	}
	if commands > maxVerificationSteps {
		return tooMany("verification commands", maxVerificationSteps)
	}
	return nil
}

func validateID(name, value string, limit int) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", name)
	}
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("%s must not have leading or trailing whitespace", name)
	}
	if len(value) > limit {
		return fmt.Errorf("%s exceeds %d bytes", name, limit)
	}
	return nil
}

func tooLarge(field string, limit int) error {
	return apierr.Invalid("TASK_PLAN_TOO_LARGE", fmt.Sprintf("%s exceeds %d bytes", field, limit), map[string]any{"field": field, "maxBytes": limit})
}

func tooMany(field string, limit int) error {
	return apierr.Invalid("TASK_PLAN_TOO_LARGE", fmt.Sprintf("Task plan exceeds %d %s", limit, field), map[string]any{"field": field, "maximum": limit})
}

func duplicatePlan() error {
	return apierr.Conflict("TASK_PLAN_ALREADY_EXISTS", "A task plan with this id already exists", nil)
}

func encodeCursor(summary domain.TaskPlanSummary) string {
	value := summary.CreatedAt.UTC().Format(time.RFC3339Nano) + "\n" + summary.ID
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodeCursor(raw string) (time.Time, string, error) {
	if raw == "" {
		return time.Time{}, "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return time.Time{}, "", invalidCursor()
	}
	createdAtRaw, id, ok := strings.Cut(string(decoded), "\n")
	if !ok || id == "" || len(id) > maxPlanIDBytes {
		return time.Time{}, "", invalidCursor()
	}
	createdAt, err := time.Parse(time.RFC3339Nano, createdAtRaw)
	if err != nil {
		return time.Time{}, "", invalidCursor()
	}
	return createdAt.UTC(), id, nil
}

func invalidCursor() error {
	return apierr.Invalid("INVALID_TASK_PLAN_CURSOR", "Task plan cursor is invalid", nil)
}
