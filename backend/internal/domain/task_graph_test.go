package domain

import (
	"fmt"
	"testing"
)

func validTaskPlan() TaskPlan {
	return TaskPlan{ID: "plan-1", ProjectID: "project-1", Title: "Ship feature", Tasks: []PlannedTask{
		{ID: "a", Title: "A", Prompt: "Do A", VerificationCommands: []string{"go test ./..."}},
		{ID: "b", Title: "B", Prompt: "Do B", DependsOn: []string{"a"}},
	}}
}

func TestTaskPlanValidateInvalidCases(t *testing.T) {
	tests := []struct {
		name string
		edit func(*TaskPlan)
		want string
	}{
		{"plan id", func(p *TaskPlan) { p.ID = " " }, "task plan id is required"},
		{"plan id whitespace", func(p *TaskPlan) { p.ID = " p" }, "task plan id must not have leading or trailing whitespace"},
		{"project id", func(p *TaskPlan) { p.ProjectID = "\t" }, "task plan project ID is required"},
		{"project id whitespace", func(p *TaskPlan) { p.ProjectID = "project " }, "task plan project ID must not have leading or trailing whitespace"},
		{"title", func(p *TaskPlan) { p.Title = "\n" }, "task plan title is required"},
		{"no tasks", func(p *TaskPlan) { p.Tasks = nil }, "task plan must contain at least one task"},
		{"phase id", func(p *TaskPlan) { p.Phases = []TaskPhase{{ID: "", Title: "Phase"}} }, "phase[0].id is required"},
		{"phase id whitespace", func(p *TaskPlan) { p.Phases = []TaskPhase{{ID: " phase", Title: "Phase"}} }, "phase[0].id must not have leading or trailing whitespace"},
		{"duplicate phase", func(p *TaskPlan) { p.Phases = []TaskPhase{{ID: "p", Title: "P"}, {ID: "p", Title: "P2"}} }, `phase[1].id "p" is duplicated`},
		{"phase title", func(p *TaskPlan) { p.Phases = []TaskPhase{{ID: "p", Title: " "}} }, "phase[0].title is required"},
		{"task id whitespace", func(p *TaskPlan) { p.Tasks[0].ID = "a " }, "task[0].id must not have leading or trailing whitespace"},
		{"task title", func(p *TaskPlan) { p.Tasks[0].Title = " " }, "task[0].title is required"},
		{"task prompt", func(p *TaskPlan) { p.Tasks[0].Prompt = "\t" }, "task[0].prompt is required"},
		{"duplicate task", func(p *TaskPlan) { p.Tasks[1].ID = "a" }, `task[1].id "a" is duplicated`},
		{"unknown phase", func(p *TaskPlan) { p.Tasks[0].PhaseID = "missing" }, `task[0].phaseId "missing" is unknown`},
		{"missing phase reference", func(p *TaskPlan) { p.Phases = []TaskPhase{{ID: "p", Title: "P"}} }, "task[0].phaseId is required when phases are defined"},
		{"unknown dependency", func(p *TaskPlan) { p.Tasks[1].DependsOn = []string{"missing"} }, `task[1].dependsOn[0] "missing" is unknown`},
		{"self dependency", func(p *TaskPlan) { p.Tasks[0].DependsOn = []string{"a"} }, "task[0].dependsOn[0] cannot depend on itself"},
		{"duplicate dependency", func(p *TaskPlan) { p.Tasks[1].DependsOn = []string{"a", "a"} }, `task[1].dependsOn[1] "a" is duplicated`},
		{"blank command", func(p *TaskPlan) { p.Tasks[0].VerificationCommands = []string{" "} }, "task[0].verificationCommands[0] must not be blank"},
		{"unlock verification", func(p *TaskPlan) { p.Tasks[0].VerificationCommands = nil }, "task[0] must have verification commands because it unlocks dependent tasks"},
		{"cycle", func(p *TaskPlan) {
			p.Tasks[0].DependsOn = []string{"b"}
			p.Tasks[1].VerificationCommands = []string{"verify"}
		}, "task plan contains a dependency cycle"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := validTaskPlan()
			tt.edit(&plan)
			if err := plan.Validate(); err == nil || err.Error() != tt.want {
				t.Fatalf("Validate() = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestTaskPlanValidateValidGraphs(t *testing.T) {
	tests := []struct {
		name string
		plan TaskPlan
	}{
		{"disconnected", TaskPlan{ID: "p", ProjectID: "project", Title: "T", Tasks: []PlannedTask{{ID: "a", Title: "A", Prompt: "A"}, {ID: "b", Title: "B", Prompt: "B"}}}},
		{"cross phase and out of order", TaskPlan{ID: "p", ProjectID: "project", Title: "T", Phases: []TaskPhase{{ID: "one", Title: "One"}, {ID: "two", Title: "Two"}}, Tasks: []PlannedTask{{ID: "b", PhaseID: "two", Title: "B", Prompt: "B", DependsOn: []string{"a"}}, {ID: "a", PhaseID: "one", Title: "A", Prompt: "A", VerificationCommands: []string{"verify"}}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.plan.Validate(); err != nil {
				t.Fatalf("Validate() = %v", err)
			}
		})
	}
}

func TestTaskPlanValidateDeepGraphIsIterative(t *testing.T) {
	const count = 10000
	plan := TaskPlan{ID: "p", ProjectID: "project", Title: "deep", Tasks: make([]PlannedTask, count)}
	for i := range plan.Tasks {
		plan.Tasks[i] = PlannedTask{ID: fmt.Sprintf("task-%d", i), Title: "task", Prompt: "prompt"}
		if i > 0 {
			plan.Tasks[i].DependsOn = []string{fmt.Sprintf("task-%d", i-1)}
		}
		if i < count-1 {
			plan.Tasks[i].VerificationCommands = []string{"verify"}
		}
	}
	if err := plan.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}
