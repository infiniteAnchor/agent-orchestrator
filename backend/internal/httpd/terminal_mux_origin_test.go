package httpd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/terminal"
)

func TestMuxOriginAllowed(t *testing.T) {
	allowed := exactAllowedOrigins([]string{"app://renderer"})
	cases := []struct {
		name   string
		origin string
		want   bool
	}{
		{name: "empty origin (native client)", origin: "", want: true},
		{name: "packaged electron", origin: "app://renderer", want: true},
		{name: "loopback vite", origin: "http://127.0.0.1:5181", want: true},
		{name: "localhost vite", origin: "http://localhost:5181", want: true},
		{name: "hostile site", origin: "http://evil.example", want: false},
		{name: "null origin", origin: "null", want: false},
		{name: "localhost lookalike", origin: "http://localhost.evil.example", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := muxOriginAllowed(tc.origin, allowed); got != tc.want {
				t.Fatalf("muxOriginAllowed(%q) = %v, want %v", tc.origin, got, tc.want)
			}
		})
	}
}

func TestMuxUpgradeRejectsForbiddenOrigin(t *testing.T) {
	mgr := terminal.NewManager(&stubSource{argv: []string{"/bin/sh"}}, nil, discardLogger())
	defer mgr.Close()

	router := newTestRouter(config.Config{AllowedOrigins: []string{"app://renderer"}}, discardLogger(), mgr)
	ts := httptest.NewServer(router)
	defer ts.Close()

	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/mux"
	_, _, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"http://evil.example"}},
	})
	if err == nil {
		t.Fatal("dial with forbidden Origin succeeded, want failure")
	}
}

func TestMuxUpgradeAllowsPackagedElectronOrigin(t *testing.T) {
	mgr := terminal.NewManager(&stubSource{argv: []string{"/bin/sh"}}, nil, discardLogger())
	defer mgr.Close()

	router := newTestRouter(config.Config{AllowedOrigins: []string{"app://renderer"}}, discardLogger(), mgr)
	ts := httptest.NewServer(router)
	defer ts.Close()

	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/mux"
	c, _, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"app://renderer"}},
	})
	if err != nil {
		t.Fatalf("dial with app://renderer: %v", err)
	}
	_ = c.Close(websocket.StatusNormalClosure, "ok")
}

func TestMuxUpgradeAllowsMissingOrigin(t *testing.T) {
	mgr := terminal.NewManager(&stubSource{argv: []string{"/bin/sh"}}, nil, discardLogger())
	defer mgr.Close()

	router := newTestRouter(config.Config{AllowedOrigins: []string{"app://renderer"}}, discardLogger(), mgr)
	ts := httptest.NewServer(router)
	defer ts.Close()

	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/mux"
	c, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial without Origin: %v", err)
	}
	_ = c.Close(websocket.StatusNormalClosure, "ok")
}
