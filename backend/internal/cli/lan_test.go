package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type lanCapture struct {
	method string
	path   string
	body   string
	called bool
}

func lanServer(t *testing.T, status int, respBody string) (*httptest.Server, *lanCapture) {
	t.Helper()
	capture := &lanCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture.called = true
		capture.method = r.Method
		capture.path = r.URL.Path
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			capture.body = string(b)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv, capture
}

const lanEnabledBody = `{"enabled":true,"hostId":"host-abc","password":"secret-pw","host":"192.168.1.42","port":3011,"tailscaleHost":"100.64.0.1","lanOnly":true,"warning":"LAN is unencrypted"}`

const lanDisabledBody = `{"enabled":false,"hostId":"host-abc","password":"","host":"","port":0,"tailscaleHost":"","lanOnly":false,"warning":"LAN is unencrypted"}`

func TestLanSubcommands(t *testing.T) {
	tests := []struct {
		name          string
		args          []string
		wantMethod    string
		wantPath      string
		respBody      string
		wantHostID    string
		wantPassword  string
		wantPinNote   bool
		wantLanOnly   bool
		wantBodySub   string
		checkJSON     bool
		wantErrSubstr string
		wantExit      int
	}{
		{
			name:       "status hits GET mobile/status",
			args:       []string{"lan", "status"},
			wantMethod: http.MethodGet,
			wantPath:   "/api/v1/mobile/status",
			respBody:   lanDisabledBody,
			wantHostID: "host-abc",
		},
		{
			name:         "enable hits POST mobile/enable with lanOnly and prints hostId and password separately",
			args:         []string{"lan", "enable"},
			wantMethod:   http.MethodPost,
			wantPath:     "/api/v1/mobile/enable",
			respBody:     lanEnabledBody,
			wantHostID:   "host-abc",
			wantPassword: "secret-pw",
			wantPinNote:  true,
			wantLanOnly:  true,
			wantBodySub:  `"lanOnly":true`,
		},
		{
			name:         "regenerate hits POST mobile/regenerate with lanOnly",
			args:         []string{"lan", "regenerate"},
			wantMethod:   http.MethodPost,
			wantPath:     "/api/v1/mobile/regenerate",
			respBody:     lanEnabledBody,
			wantHostID:   "host-abc",
			wantPassword: "secret-pw",
			wantPinNote:  true,
			wantLanOnly:  true,
			wantBodySub:  `"lanOnly":true`,
		},
		{
			name:       "disable hits POST mobile/disable",
			args:       []string{"lan", "disable"},
			wantMethod: http.MethodPost,
			wantPath:   "/api/v1/mobile/disable",
			respBody:   lanDisabledBody,
			wantHostID: "host-abc",
		},
		{
			name:          "daemon error envelope is surfaced",
			args:          []string{"lan", "enable"},
			wantMethod:    http.MethodPost,
			wantPath:      "/api/v1/mobile/enable",
			respBody:      `{"message":"bind failed","code":"MOBILE_ENABLE","requestId":"req-lan-1"}`,
			wantErrSubstr: "MOBILE_ENABLE",
			wantExit:      1,
			wantBodySub:   `"lanOnly":true`,
		},
		{
			name:         "json includes hostId and password as distinct keys",
			args:         []string{"lan", "enable", "--json"},
			wantMethod:   http.MethodPost,
			wantPath:     "/api/v1/mobile/enable",
			respBody:     lanEnabledBody,
			wantHostID:   "host-abc",
			wantPassword: "secret-pw",
			wantLanOnly:  true,
			wantBodySub:  `"lanOnly":true`,
			checkJSON:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := setConfigEnv(t)
			status := http.StatusOK
			if tc.wantErrSubstr != "" {
				status = http.StatusInternalServerError
			}
			srv, capture := lanServer(t, status, tc.respBody)
			writeRunFileFor(t, cfg, srv)

			out, errOut, err := executeCLI(t, Deps{
				ProcessAlive: func(int) bool { return true },
			}, tc.args...)

			if tc.wantErrSubstr != "" {
				if err == nil {
					t.Fatal("expected daemon error")
				}
				if got := ExitCode(err); got != tc.wantExit {
					t.Fatalf("ExitCode = %d, want %d", got, tc.wantExit)
				}
				combined := err.Error() + errOut
				if !strings.Contains(combined, tc.wantErrSubstr) {
					t.Fatalf("error missing %q: %v\nstderr=%s", tc.wantErrSubstr, err, errOut)
				}
				if !strings.Contains(combined, "req-lan-1") {
					t.Fatalf("error missing request id: %v\nstderr=%s", err, errOut)
				}
				if capture.path != tc.wantPath || capture.method != tc.wantMethod {
					t.Fatalf("request = %s %s, want %s %s", capture.method, capture.path, tc.wantMethod, tc.wantPath)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v\nstderr=%s", err, errOut)
			}
			if !capture.called {
				t.Fatal("daemon was not contacted")
			}
			if capture.method != tc.wantMethod {
				t.Errorf("method = %q, want %q", capture.method, tc.wantMethod)
			}
			if capture.path != tc.wantPath {
				t.Errorf("path = %q, want %q", capture.path, tc.wantPath)
			}
			if tc.wantBodySub != "" && !strings.Contains(capture.body, tc.wantBodySub) {
				t.Fatalf("request body missing %q: %q", tc.wantBodySub, capture.body)
			}

			if tc.checkJSON {
				var got lanStatusDTO
				if err := json.Unmarshal([]byte(out), &got); err != nil {
					t.Fatalf("decode JSON: %v\noutput=%s", err, out)
				}
				if got.HostID != tc.wantHostID {
					t.Errorf("hostId = %q, want %q", got.HostID, tc.wantHostID)
				}
				if got.Password != tc.wantPassword {
					t.Errorf("password = %q, want %q", got.Password, tc.wantPassword)
				}
				if !got.Enabled {
					t.Error("enabled = false, want true")
				}
				if got.LanOnly != tc.wantLanOnly {
					t.Errorf("lanOnly = %v, want %v", got.LanOnly, tc.wantLanOnly)
				}
				// Distinct keys, not a concatenated blob.
				if strings.Contains(out, tc.wantHostID+tc.wantPassword) ||
					strings.Contains(out, tc.wantPassword+tc.wantHostID) {
					t.Fatalf("JSON concatenated hostId and password: %s", out)
				}
				return
			}

			if !strings.Contains(out, "host id: "+tc.wantHostID) {
				t.Fatalf("output missing labeled host id: %q", out)
			}
			if tc.wantLanOnly && !strings.Contains(out, "mode: lan-only") {
				t.Fatalf("output missing lan-only mode note: %q", out)
			}
			if tc.wantPassword != "" {
				if !strings.Contains(out, "password: "+tc.wantPassword) {
					t.Fatalf("output missing labeled password: %q", out)
				}
				// Must not concatenate host id and password into one field.
				if strings.Contains(out, tc.wantHostID+tc.wantPassword) ||
					strings.Contains(out, tc.wantPassword+"@"+tc.wantHostID) {
					t.Fatalf("host id and password appear concatenated: %q", out)
				}
			} else if strings.Contains(out, "password:") {
				t.Fatalf("status/disable invented a password line: %q", out)
			}
			if tc.wantPinNote {
				if !strings.Contains(out, "Pin the host id") {
					t.Fatalf("missing pin-before-password note: %q", out)
				}
			}
		})
	}
}

func TestLanExtraArgsAreUsageErrors(t *testing.T) {
	setConfigEnv(t)
	for _, args := range [][]string{
		{"lan", "status", "extra"},
		{"lan", "enable", "extra"},
		{"lan", "disable", "extra"},
		{"lan", "regenerate", "extra"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, _, err := executeCLI(t, Deps{}, args...)
			if err == nil {
				t.Fatal("expected usage error")
			}
			if got := ExitCode(err); got != 2 {
				t.Fatalf("ExitCode(%v) = %d, want 2", err, got)
			}
		})
	}
}

func TestLanCommandIsVisible(t *testing.T) {
	root := NewRootCommand(Deps{})
	lanCmd, _, err := root.Find([]string{"lan"})
	if err != nil {
		t.Fatalf("Find lan: %v", err)
	}
	if lanCmd.Hidden {
		t.Fatal("lan command must be visible")
	}
	for _, name := range []string{"status", "enable", "disable", "regenerate"} {
		sub, _, err := root.Find([]string{"lan", name})
		if err != nil {
			t.Fatalf("Find lan %s: %v", name, err)
		}
		if sub.Hidden {
			t.Fatalf("lan %s must be visible", name)
		}
	}
}
