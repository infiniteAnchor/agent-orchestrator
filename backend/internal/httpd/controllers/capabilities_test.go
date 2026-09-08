package controllers_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
)

func TestGetCapabilitiesReturnsRemoteDesktopContract(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)

	res, err := http.Get(srv.URL + "/api/v1/capabilities")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", res.StatusCode)
	}
	var got controllers.CapabilitiesResponse
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.APIVersion != controllers.MobileAPIVersion {
		t.Fatalf("apiVersion = %d, want %d", got.APIVersion, controllers.MobileAPIVersion)
	}
	if !got.RemoteDesktop.Supported {
		t.Fatal("remoteDesktop.supported = false, want true")
	}
	if got.RemoteDesktop.ContractVersion != controllers.RemoteDesktopContractVersion {
		t.Fatalf("contractVersion = %d, want %d", got.RemoteDesktop.ContractVersion, controllers.RemoteDesktopContractVersion)
	}
	if got.RemoteDesktop.Auth != "bearer-password" {
		t.Fatalf("auth = %q, want bearer-password", got.RemoteDesktop.Auth)
	}
	wantTransports := map[string]bool{"http": true, "sse": true, "mux": true}
	if len(got.RemoteDesktop.Transports) != len(wantTransports) {
		t.Fatalf("transports = %v, want http/sse/mux", got.RemoteDesktop.Transports)
	}
	for _, tr := range got.RemoteDesktop.Transports {
		if !wantTransports[tr] {
			t.Fatalf("unexpected transport %q in %v", tr, got.RemoteDesktop.Transports)
		}
	}
}

// Capabilities must stay off the unauthenticated identity probe (ADR 0003).
func TestCapabilitiesIsNotFoldedIntoIdentityProbe(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{
		HostID: "h_abc",
	}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)

	res, err := http.Get(srv.URL + "/api/v1/identity")
	if err != nil {
		t.Fatalf("GET identity: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	var got map[string]any
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for k := range got {
		if k != "hostId" && k != "apiVersion" {
			t.Fatalf("identity probe leaked field %q; capabilities belong on /api/v1/capabilities", k)
		}
	}
}

func TestCapabilitiesPathIsNotLANBlocked(t *testing.T) {
	if httpd.IsLANControlBlockedPathForTest("/api/v1/capabilities") {
		t.Fatal("/api/v1/capabilities must remain reachable on the LAN listener for remote desktop")
	}
}
