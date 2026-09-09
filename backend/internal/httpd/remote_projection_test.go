package httpd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/reqctx"
	"github.com/aoagents/agent-orchestrator/backend/internal/mobilebridge"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/shellterm"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/systemcheck"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/systeminstall"
)

func TestLANHealthProbesOmitAbsolutePaths(t *testing.T) {
	inner := NewRouterWithControl(config.Config{
		AllowedOrigins:          []string{"app://renderer"},
		StartupWorkingDirectory: "/startup",
	}, discardLogger(), nil, APIDeps{}, ControlDeps{})
	st := &authState{}
	st.setHash(mobilebridge.HashPassword("secret12"))
	m := NewLANManager(inner, st, 0, slog.Default(), nil)
	port, err := m.Start(0)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer m.Stop(context.Background())

	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/readyz", port), nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret12")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"executablePath", "workingDirectory", "startupWorkingDirectory", "appImagePath"} {
		if _, ok := got[key]; ok {
			t.Fatalf("LAN /readyz leaked %q: %#v", key, got)
		}
	}
	if got["status"] != "ready" {
		t.Fatalf("status = %#v, want ready", got["status"])
	}
}

func TestLoopbackHealthProbesKeepAbsolutePaths(t *testing.T) {
	router := newTestRouter(config.Config{StartupWorkingDirectory: "/startup"}, discardLogger(), nil)
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["executablePath"] == nil || got["executablePath"] == "" {
		t.Fatalf("loopback /readyz missing executablePath: %#v", got)
	}
	if got["startupWorkingDirectory"] != "/startup" {
		t.Fatalf("startupWorkingDirectory = %#v, want /startup", got["startupWorkingDirectory"])
	}
}

func TestLANRequestContextIsSetOnLANListener(t *testing.T) {
	var sawLAN bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawLAN = reqctx.IsLAN(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	st := &authState{}
	st.setHash(mobilebridge.HashPassword("secret12"))
	m := NewLANManager(inner, st, 0, slog.Default(), nil)
	port, err := m.Start(0)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer m.Stop(context.Background())

	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/x", port), nil)
	req.Header.Set("Authorization", "Bearer secret12")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if !sawLAN {
		t.Fatal("LAN handler did not see reqctx.IsLAN")
	}
}

type stubSystemChecks struct {
	report      systemcheck.Report
	requirement systemcheck.Requirement
}

func (s stubSystemChecks) CheckStartup(context.Context) (systemcheck.Report, error) {
	return s.report, nil
}

func (s stubSystemChecks) CheckGitHubAuth(context.Context) (systemcheck.Requirement, error) {
	return s.requirement, nil
}

func (s stubSystemChecks) OpenGitHubAuthTerminal(context.Context) (shellterm.ShellTerminal, error) {
	return shellterm.ShellTerminal{}, errors.New("not used")
}

type stubInstaller struct {
	plans []systeminstall.AgentPlan
	jobs  []systeminstall.Job
}

func (s stubInstaller) Start(context.Context, systeminstall.Target) (systeminstall.Job, error) {
	return systeminstall.Job{}, nil
}

func (s stubInstaller) StartAgentOperation(context.Context, systeminstall.Target, string, systeminstall.AgentOperation) (systeminstall.Job, error) {
	return systeminstall.Job{}, nil
}

func (s stubInstaller) Status(context.Context, systeminstall.Target) (systeminstall.Job, error) {
	return systeminstall.Job{}, nil
}

func (s stubInstaller) AgentPlans(context.Context) ([]systeminstall.AgentPlan, error) {
	return s.plans, nil
}

func (s stubInstaller) AgentJobs(context.Context) ([]systeminstall.Job, error) {
	return s.jobs, nil
}

func (s stubInstaller) Verify(context.Context, systeminstall.Target) (systeminstall.Job, error) {
	return systeminstall.Job{}, nil
}

// TestLANRoutesProjectHostPathsEndToEnd drives the real router through the LAN
// listener so a projection that is written but never wired to its handler still
// fails the build.
func TestLANRoutesProjectHostPathsEndToEnd(t *testing.T) {
	inner := NewRouterWithControl(config.Config{AllowedOrigins: []string{"app://renderer"}}, discardLogger(), nil, APIDeps{
		SystemChecks: stubSystemChecks{
			report: systemcheck.Report{
				Ready:        true,
				Requirements: []systemcheck.Requirement{{ID: "git", Satisfied: true, Required: true, Detail: "/usr/bin/git"}},
			},
			requirement: systemcheck.Requirement{ID: "github-auth", Satisfied: true, Detail: "/usr/bin/gh"},
		},
		Installer: stubInstaller{
			plans: []systeminstall.AgentPlan{{
				AgentID:             "codex",
				ExpectedDestination: "/usr/local/bin/codex",
				Reason:              "installs into /usr/local/lib/node_modules",
			}},
			jobs: []systeminstall.Job{{
				Target:              systeminstall.TargetCodex,
				Status:              systeminstall.StatusFailed,
				ExpectedDestination: "/usr/local/bin/codex",
				Output:              "npm ERR! EACCES /usr/local/lib/node_modules",
			}},
		},
	}, ControlDeps{})
	st := &authState{}
	st.setHash(mobilebridge.HashPassword("secret12"))
	m := NewLANManager(inner, st, 0, slog.Default(), nil)
	port, err := m.Start(0)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer m.Stop(context.Background())

	for _, path := range []string{
		"/api/v1/system/requirements",
		"/api/v1/system/github-auth",
		"/api/v1/agents/installers",
		"/api/v1/agents/install-jobs",
	} {
		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
		if err != nil {
			t.Fatalf("%s: request: %v", path, err)
		}
		req.Header.Set("Authorization", "Bearer secret12")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: GET: %v", path, err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("%s: read: %v", path, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d body=%s", path, resp.StatusCode, body)
		}
		if strings.Contains(string(body), "/usr/") || strings.Contains(string(body), "/usr/local") {
			t.Fatalf("%s leaked a host path: %s", path, body)
		}
	}
}
