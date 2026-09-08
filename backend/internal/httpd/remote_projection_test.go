package httpd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/reqctx"
	"github.com/aoagents/agent-orchestrator/backend/internal/mobilebridge"
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
