package httpd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
)

type recoveryGate struct{ ready bool }

func (g recoveryGate) Ready() bool { return g.ready }

func TestReadyzStaysPendingUntilTaskRecoveryFinishes(t *testing.T) {
	router := NewRouterWithControl(config.Config{StartupWorkingDirectory: "/startup"}, discardLogger(), nil, APIDeps{
		TaskRecovery: recoveryGate{ready: false},
	}, ControlDeps{})
	srv := httptest.NewServer(router)
	defer srv.Close()

	health, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d, want 200 while task recovery is pending", health.StatusCode)
	}

	ready, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer ready.Body.Close()
	body, _ := io.ReadAll(ready.Body)
	if ready.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d, want 503; body %s", ready.StatusCode, body)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["status"] != "task_recovery_pending" || payload["taskRecovery"] != "pending" {
		t.Fatalf("payload = %#v", payload)
	}
	if payload["executablePath"] == "" {
		t.Fatal("loopback readyz should keep the daemon identity path while recovery is pending")
	}
}

func TestReadyzCompletesAfterTaskRecovery(t *testing.T) {
	router := NewRouterWithControl(config.Config{}, discardLogger(), nil, APIDeps{
		TaskRecovery: recoveryGate{ready: true},
	}, ControlDeps{})
	srv := httptest.NewServer(router)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["status"] != "ready" || payload["taskRecovery"] != "complete" {
		t.Fatalf("payload = %#v", payload)
	}
}
