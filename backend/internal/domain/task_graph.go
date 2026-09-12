package domain

import (
	"fmt"
	"strings"
)

// TaskPlan is a proposed task graph for one project.
type TaskPlan struct {
	ID        string
	ProjectID string
	Title     string
	Phases    []TaskPhase
	Tasks     []PlannedTask
}

// TaskPhase groups tasks for presentation and planning. Dependencies remain
// explicit on PlannedTask and do not follow phase ordering.
type TaskPhase struct {
	ID    string
	Title string
}

// PlannedTask is a node in a TaskPlan dependency graph.
type PlannedTask struct {
	ID                   string
	PhaseID              string
	Title                string
	Prompt               string
	DependsOn            []string
	VerificationCommands []string
}

// Validate checks the task graph without consulting persistence or runtime state.
func (p TaskPlan) Validate() error {
	if strings.TrimSpace(p.ID) == "" {
		return fmt.Errorf("task plan id is required")
	}
	if p.ID != strings.TrimSpace(p.ID) {
		return fmt.Errorf("task plan id must not have leading or trailing whitespace")
	}
	if strings.TrimSpace(p.ProjectID) == "" {
		return fmt.Errorf("task plan project ID is required")
	}
	if p.ProjectID != strings.TrimSpace(p.ProjectID) {
		return fmt.Errorf("task plan project ID must not have leading or trailing whitespace")
	}
	if strings.TrimSpace(p.Title) == "" {
		return fmt.Errorf("task plan title is required")
	}
	if len(p.Tasks) == 0 {
		return fmt.Errorf("task plan must contain at least one task")
	}

	phases := make(map[string]struct{}, len(p.Phases))
	for i, phase := range p.Phases {
		if strings.TrimSpace(phase.ID) == "" {
			return fmt.Errorf("phase[%d].id is required", i)
		}
		if phase.ID != strings.TrimSpace(phase.ID) {
			return fmt.Errorf("phase[%d].id must not have leading or trailing whitespace", i)
		}
		if strings.TrimSpace(phase.Title) == "" {
			return fmt.Errorf("phase[%d].title is required", i)
		}
		if _, exists := phases[phase.ID]; exists {
			return fmt.Errorf("phase[%d].id %q is duplicated", i, phase.ID)
		}
		phases[phase.ID] = struct{}{}
	}

	tasks := make(map[string]int, len(p.Tasks))
	for i, task := range p.Tasks {
		if strings.TrimSpace(task.ID) == "" {
			return fmt.Errorf("task[%d].id is required", i)
		}
		if task.ID != strings.TrimSpace(task.ID) {
			return fmt.Errorf("task[%d].id must not have leading or trailing whitespace", i)
		}
		if strings.TrimSpace(task.Title) == "" {
			return fmt.Errorf("task[%d].title is required", i)
		}
		if strings.TrimSpace(task.Prompt) == "" {
			return fmt.Errorf("task[%d].prompt is required", i)
		}
		if len(phases) > 0 && task.PhaseID == "" {
			return fmt.Errorf("task[%d].phaseId is required when phases are defined", i)
		}
		if task.PhaseID != "" {
			if task.PhaseID != strings.TrimSpace(task.PhaseID) {
				return fmt.Errorf("task[%d].phaseId must not have leading or trailing whitespace", i)
			}
			if _, exists := phases[task.PhaseID]; !exists {
				return fmt.Errorf("task[%d].phaseId %q is unknown", i, task.PhaseID)
			}
		}
		if _, exists := tasks[task.ID]; exists {
			return fmt.Errorf("task[%d].id %q is duplicated", i, task.ID)
		}
		tasks[task.ID] = i
		for j, command := range task.VerificationCommands {
			if strings.TrimSpace(command) == "" {
				return fmt.Errorf("task[%d].verificationCommands[%d] must not be blank", i, j)
			}
		}
	}

	dependents := make([][]int, len(p.Tasks))
	indegree := make([]int, len(p.Tasks))
	for i, task := range p.Tasks {
		seen := make(map[string]struct{}, len(task.DependsOn))
		for j, dependency := range task.DependsOn {
			if dependency == task.ID {
				return fmt.Errorf("task[%d].dependsOn[%d] cannot depend on itself", i, j)
			}
			if _, exists := seen[dependency]; exists {
				return fmt.Errorf("task[%d].dependsOn[%d] %q is duplicated", i, j, dependency)
			}
			seen[dependency] = struct{}{}
			dependencyIndex, exists := tasks[dependency]
			if !exists {
				return fmt.Errorf("task[%d].dependsOn[%d] %q is unknown", i, j, dependency)
			}
			indegree[i]++
			dependents[dependencyIndex] = append(dependents[dependencyIndex], i)
		}
	}

	for i, downstream := range dependents {
		if len(downstream) > 0 && len(p.Tasks[i].VerificationCommands) == 0 {
			return fmt.Errorf("task[%d] must have verification commands because it unlocks dependent tasks", i)
		}
	}

	queue := make([]int, 0, len(p.Tasks))
	for i, degree := range indegree {
		if degree == 0 {
			queue = append(queue, i)
		}
	}
	visited := 0
	for head := 0; head < len(queue); head++ {
		i := queue[head]
		visited++
		for _, dependent := range dependents[i] {
			indegree[dependent]--
			if indegree[dependent] == 0 {
				queue = append(queue, dependent)
			}
		}
	}
	if visited != len(p.Tasks) {
		return fmt.Errorf("task plan contains a dependency cycle")
	}
	return nil
}
