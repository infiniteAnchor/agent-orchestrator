package controllers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/reqctx"
	projectsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/project"
)

func TestProjectSummariesForWireOmitsPathOnLAN(t *testing.T) {
	in := []projectsvc.Summary{{
		ID: "demo", Name: "Demo", Path: "/secret/repo", Kind: domain.ProjectKindSingleRepo,
	}}
	got := projectSummariesForWire(reqctx.WithLAN(context.Background()), in)
	if got[0].Path != "" {
		t.Fatalf("LAN path = %q, want empty", got[0].Path)
	}
	if in[0].Path != "/secret/repo" {
		t.Fatal("helper mutated the input slice element")
	}
	raw, err := json.Marshal(got[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := asMap["path"]; ok {
		t.Fatalf("LAN summary JSON leaked path: %s", raw)
	}
}

func TestProjectForWireKeepsPathOnLoopback(t *testing.T) {
	p := projectsvc.Project{ID: "demo", Path: "/secret/repo"}
	got := projectForWire(context.Background(), p)
	if got.Path != "/secret/repo" {
		t.Fatalf("loopback path = %q", got.Path)
	}
}

func TestGetProjectResponseForWireOmitsDegradedPathOnLAN(t *testing.T) {
	d := projectsvc.Degraded{ID: "demo", Path: "/secret/repo", ResolveError: "boom"}
	resp := GetProjectResponse{
		Status:  "degraded",
		Project: ProjectOrDegraded{Degraded: &d},
	}
	got := getProjectResponseForWire(reqctx.WithLAN(context.Background()), resp)
	if got.Project.Degraded == nil || got.Project.Degraded.Path != "" {
		t.Fatalf("LAN degraded = %#v", got.Project.Degraded)
	}
	if d.Path != "/secret/repo" {
		t.Fatal("helper mutated the original degraded value")
	}
}
