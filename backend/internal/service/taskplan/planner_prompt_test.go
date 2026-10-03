package taskplan

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPlannerPromptExampleMatchesGraphContract(t *testing.T) {
	prompt := plannerPrompt("Create a result")
	var example string
	for _, line := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(line, "{\"title\":") {
			example = line
			break
		}
	}
	var input CreateInput
	if err := json.Unmarshal([]byte(example), &input); err != nil {
		t.Fatalf("planner example is not task-plan JSON: %v", err)
	}
	input.ID = "proposal-example"
	if _, err := graphFromInput("project-example", input); err != nil {
		t.Fatalf("planner example violates graph contract: %v", err)
	}
}
