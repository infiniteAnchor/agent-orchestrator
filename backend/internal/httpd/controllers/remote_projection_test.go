package controllers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/reqctx"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	projectsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/project"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/systemcheck"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/systeminstall"
)

func lanCtx() context.Context   { return reqctx.WithLAN(context.Background()) }
func localCtx() context.Context { return context.Background() }

func TestInitializeRepositoryForWireOmitsPathOnLAN(t *testing.T) {
	in := projectsvc.InitializeRepositoryResult{Path: "/secret/repo"}
	got := initializeRepositoryForWire(lanCtx(), in)
	if got.Path != "" {
		t.Fatalf("LAN path = %q, want empty", got.Path)
	}
	if in.Path != "/secret/repo" {
		t.Fatal("helper mutated the input")
	}
	if kept := initializeRepositoryForWire(localCtx(), in); kept.Path != "/secret/repo" {
		t.Fatalf("loopback path = %q", kept.Path)
	}
}

func TestRequirementForWireRedactsDetailOnLAN(t *testing.T) {
	requirement := systemcheck.Requirement{ID: "git", Label: "git", Satisfied: true, Detail: "/usr/bin/git"}
	got := requirementForWire(lanCtx(), requirement)
	if got.Detail != "" {
		t.Fatalf("LAN detail = %q, want empty", got.Detail)
	}
	if !got.Satisfied || got.ID != "git" {
		t.Fatalf("projection dropped non-path facts: %#v", got)
	}
	if kept := requirementForWire(localCtx(), requirement); kept.Detail != "/usr/bin/git" {
		t.Fatalf("loopback detail = %q", kept.Detail)
	}
}

func TestReportForWireKeepsSafeMessagesOnLAN(t *testing.T) {
	report := systemcheck.Report{
		Ready: false,
		Requirements: []systemcheck.Requirement{
			{ID: "git", Detail: "git was not found on PATH."},
			{ID: "tmux", Detail: "AO's bundled tmux is missing or not executable: /opt/ao/tmux"},
			{ID: "gh", Satisfied: true, Detail: "/usr/bin/gh"},
		},
	}
	got := reportForWire(lanCtx(), report)
	if got.Requirements[0].Detail != "git was not found on PATH." {
		t.Fatalf("safe message = %q", got.Requirements[0].Detail)
	}
	if got.Requirements[1].Detail != "AO's bundled tmux is missing or not executable: <path>" {
		t.Fatalf("embedded path = %q", got.Requirements[1].Detail)
	}
	if got.Requirements[2].Detail != "" {
		t.Fatalf("satisfied detail = %q", got.Requirements[2].Detail)
	}
	if report.Requirements[2].Detail != "/usr/bin/gh" {
		t.Fatal("helper mutated the input report")
	}
}

func TestAgentPlanForWireOmitsHostDestinationsOnLAN(t *testing.T) {
	plan := systeminstall.AgentPlan{
		AgentID:             "codex",
		Command:             "npm install -g @openai/codex",
		Reason:              "installs into /usr/local/lib/node_modules",
		ExpectedDestination: "/usr/local/bin/codex",
		Methods: []systeminstall.AgentInstallMethod{{
			ID:                  "npm",
			Command:             "npm install -g @openai/codex",
			Reason:              "resolved /usr/local/bin/npm",
			ReinstallCommand:    "npm install -g @openai/codex",
			ReinstallReason:     "existing binary at /usr/local/bin/codex",
			ExpectedDestination: "/usr/local/bin/codex",
		}},
	}
	got := agentPlanForWire(lanCtx(), plan)
	if got.ExpectedDestination != "" {
		t.Fatalf("LAN expectedDestination = %q", got.ExpectedDestination)
	}
	if got.Reason != "installs into <path>" {
		t.Fatalf("LAN reason = %q", got.Reason)
	}
	if got.Command != plan.Command {
		t.Fatalf("LAN command = %q, want unchanged", got.Command)
	}
	method := got.Methods[0]
	if method.ExpectedDestination != "" {
		t.Fatalf("LAN method destination = %q", method.ExpectedDestination)
	}
	if method.Reason != "resolved <path>" || method.ReinstallReason != "existing binary at <path>" {
		t.Fatalf("LAN method reasons = %q / %q", method.Reason, method.ReinstallReason)
	}
	if plan.ExpectedDestination != "/usr/local/bin/codex" || plan.Methods[0].ExpectedDestination != "/usr/local/bin/codex" {
		t.Fatal("helper mutated the input plan")
	}
	if kept := agentPlanForWire(localCtx(), plan); kept.ExpectedDestination != "/usr/local/bin/codex" {
		t.Fatalf("loopback destination = %q", kept.ExpectedDestination)
	}
}

func TestInstallJobForWireRedactsOutputOnLAN(t *testing.T) {
	job := systeminstall.Job{
		Target:              systeminstall.TargetCodex,
		Status:              systeminstall.StatusFailed,
		ExpectedDestination: "/usr/local/bin/codex",
		Output:              "npm ERR! EACCES /usr/local/lib/node_modules",
		Error:               "exit status 1: /usr/local/bin/npm",
	}
	got := installJobForWire(lanCtx(), job)
	if got.ExpectedDestination != "" {
		t.Fatalf("LAN destination = %q", got.ExpectedDestination)
	}
	if got.Output != "npm ERR! EACCES <path>" {
		t.Fatalf("LAN output = %q", got.Output)
	}
	if got.Error != "exit status 1: <path>" {
		t.Fatalf("LAN error = %q", got.Error)
	}
	if got.Status != systeminstall.StatusFailed || got.Target != systeminstall.TargetCodex {
		t.Fatalf("projection dropped job facts: %#v", got)
	}
	if kept := installJobForWire(localCtx(), job); kept.Output != job.Output {
		t.Fatalf("loopback output = %q", kept.Output)
	}
}

func TestModelCatalogForWireRedactsWarningOnLAN(t *testing.T) {
	catalog := ports.AgentModelCatalog{
		AgentID: "codex",
		Warning: "discovery failed: fork/exec /home/u/.npm/bin/codex: no such file",
	}
	got := modelCatalogForWire(lanCtx(), catalog)
	if got.Warning != "discovery failed: fork/exec <path>: no such file" {
		t.Fatalf("LAN warning = %q", got.Warning)
	}
	if kept := modelCatalogForWire(localCtx(), catalog); kept.Warning != catalog.Warning {
		t.Fatalf("loopback warning = %q", kept.Warning)
	}
}

func TestActivityDetailPayloadHidesCwdOnLAN(t *testing.T) {
	activity := domain.ConversationActivity{
		Kind:          domain.ActivityKindCommand,
		Detail:        []byte(`{"command":"npm test","cwd":"/home/u/worktrees/ao-1","rawCommand":"/usr/bin/npm test"}`),
		CommandOutput: "open /home/u/worktrees/ao-1/out.log: no such file",
	}
	got := activityDetailPayload(lanCtx(), activity)
	if _, ok := got["cwd"]; ok {
		t.Fatalf("LAN detail leaked cwd: %#v", got)
	}
	if got["command"] != "npm test" {
		t.Fatalf("LAN command = %#v", got["command"])
	}
	if got["rawCommand"] != "<path> test" {
		t.Fatalf("LAN rawCommand = %#v", got["rawCommand"])
	}
	if got["output"] != "open <path>: no such file" {
		t.Fatalf("LAN output = %#v", got["output"])
	}

	kept := activityDetailPayload(localCtx(), activity)
	if kept["cwd"] != "/home/u/worktrees/ao-1" {
		t.Fatalf("loopback cwd = %#v", kept["cwd"])
	}
	if kept["output"] != activity.CommandOutput {
		t.Fatalf("loopback output = %#v", kept["output"])
	}
	// The decoded detail map is rebuilt per call; the stored JSON must be intact.
	var stored map[string]any
	if err := json.Unmarshal(activity.Detail, &stored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if stored["cwd"] != "/home/u/worktrees/ao-1" {
		t.Fatal("helper mutated the stored detail JSON")
	}
}
