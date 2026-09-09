package controllers

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/remotewire"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/reqctx"
	projectsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/project"
)

// projectForWire returns a project read-model safe for the request's listener.
// LAN responses omit absolute host paths; loopback keeps them for local desktop.
func projectForWire(ctx context.Context, p projectsvc.Project) projectsvc.Project {
	if !reqctx.IsLAN(ctx) {
		return p
	}
	p.Path = ""
	return p
}

// initializeRepositoryForWire omits the initialized repository path on LAN.
func initializeRepositoryForWire(ctx context.Context, result projectsvc.InitializeRepositoryResult) projectsvc.InitializeRepositoryResult {
	result.Path = remotewire.Path(ctx, result.Path)
	return result
}

func projectSummariesForWire(ctx context.Context, in []projectsvc.Summary) []projectsvc.Summary {
	if !reqctx.IsLAN(ctx) || len(in) == 0 {
		return in
	}
	out := make([]projectsvc.Summary, len(in))
	copy(out, in)
	for i := range out {
		out[i].Path = ""
	}
	return out
}

func getProjectResponseForWire(ctx context.Context, resp GetProjectResponse) GetProjectResponse {
	if !reqctx.IsLAN(ctx) {
		return resp
	}
	switch {
	case resp.Project.Project != nil:
		p := *resp.Project.Project
		p.Path = ""
		resp.Project.Project = &p
	case resp.Project.Degraded != nil:
		d := *resp.Project.Degraded
		d.Path = ""
		resp.Project.Degraded = &d
	}
	return resp
}
