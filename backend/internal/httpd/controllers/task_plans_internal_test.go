package controllers

import (
	"context"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/reqctx"
)

func TestTaskPlanWireProjectionRedactsLANHostPaths(t *testing.T) {
	plan := domain.TaskPlan{
		ID: "p", ProjectID: "project", Title: "Plan in /srv/private/repo",
		Phases: []domain.TaskPhase{{ID: "phase", Title: "Build /srv/private/repo"}},
		Tasks: []domain.PlannedTask{{
			ID: "task", Title: "Edit /srv/private/repo/main.go", Prompt: "Read /srv/private/repo/main.go",
			VerificationCommands: []string{"go test /srv/private/repo/..."},
		}},
	}
	local := taskPlanForWire(context.Background(), plan)
	if !strings.Contains(local.Tasks[0].Prompt, "/srv/private") {
		t.Fatalf("loopback projection lost path: %+v", local)
	}
	remote := taskPlanForWire(reqctx.WithLAN(context.Background()), plan)
	encoded := remote.Title + remote.Phases[0].Title + remote.Tasks[0].Title + remote.Tasks[0].Prompt + remote.Tasks[0].VerificationCommands[0]
	if strings.Contains(encoded, "/srv/private") {
		t.Fatalf("LAN projection leaked host path: %+v", remote)
	}
}
