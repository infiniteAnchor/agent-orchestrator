package remotewire

import (
	"context"
	"reflect"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/reqctx"
)

func lan() context.Context   { return reqctx.WithLAN(context.Background()) }
func local() context.Context { return context.Background() }

func TestPath(t *testing.T) {
	if got := Path(local(), "/home/u/repo"); got != "/home/u/repo" {
		t.Fatalf("loopback Path = %q", got)
	}
	if got := Path(lan(), "/home/u/repo"); got != "" {
		t.Fatalf("LAN Path = %q, want empty", got)
	}
	if got := Path(lan(), ""); got != "" {
		t.Fatalf("LAN Path empty = %q", got)
	}
}

func TestTextLoopbackPassthrough(t *testing.T) {
	const in = "tmux at /usr/local/bin/tmux"
	if got := Text(local(), in); got != in {
		t.Fatalf("loopback Text = %q, want %q", got, in)
	}
}

func TestTextOnLAN(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"exact absolute path", "/usr/bin/git", ""},
		{"exact home relative path", "~/bin/tmux", ""},
		{"exact windows path", `C:\Users\me\bin\git.exe`, ""},
		{"embedded path", "tmux was not found on PATH; checked /usr/local/bin", "tmux was not found on PATH; checked <path>"},
		{"path with trailing period", "failed to open /var/log/ao.log.", "failed to open <path>."},
		{"quoted path", `open "/home/u/My Project" failed`, `open "<path>" failed`},
		{"file url", "see file:///home/u/repo/README.md for details", "see <path> for details"},
		{"unc share", `copy \\server\share\file.txt first`, `copy <path> first`},
		{"two paths", "from /a/b to /c/d", "from <path> to <path>"},
		{"empty", "", ""},
		{"no path", "git was not found on PATH.", "git was not found on PATH."},
		{"relative path untouched", "src/main.go is missing", "src/main.go is missing"},
		{"url untouched", "see https://example.com/docs/setup for help", "see https://example.com/docs/setup for help"},
		{"date untouched", "released 2024/01/02 upstream", "released 2024/01/02 upstream"},
		{"lone slash untouched", "a / b", "a / b"},
		{"double slash untouched", "// a comment", "// a comment"},
		{"comma stops the path", "check /tmp/x, then retry", "check <path>, then retry"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Text(lan(), tt.in); got != tt.want {
				t.Fatalf("Text(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestMapRedactsRecursively(t *testing.T) {
	in := map[string]any{
		"path":  "/home/u/repo",
		"note":  "checked /usr/bin/git",
		"count": 3,
		"nested": map[string]any{
			"repoPath": "/home/u/repo",
		},
		"list": []any{
			"/home/u/repo",
			map[string]any{"error": "open /home/u/repo: permission denied"},
		},
		"names":     []string{"/home/u/repo"},
		"labels":    map[string]string{"root": "/home/u/repo"},
		"untouched": true,
	}
	got := Map(lan(), in)
	if got["path"] != "" {
		t.Fatalf("path = %#v, want empty", got["path"])
	}
	if got["note"] != "checked <path>" {
		t.Fatalf("note = %#v", got["note"])
	}
	if got["count"] != 3 {
		t.Fatalf("count = %#v, want 3", got["count"])
	}
	if nested := got["nested"].(map[string]any); nested["repoPath"] != "" {
		t.Fatalf("nested repoPath = %#v", nested["repoPath"])
	}
	list := got["list"].([]any)
	if list[0] != "" {
		t.Fatalf("list[0] = %#v", list[0])
	}
	if list[1].(map[string]any)["error"] != "open <path>: permission denied" {
		t.Fatalf("list[1].error = %#v", list[1])
	}
	if names := got["names"].([]string); names[0] != "" {
		t.Fatalf("names[0] = %#v", names[0])
	}
	if labels := got["labels"].(map[string]string); labels["root"] != "" {
		t.Fatalf("labels[root] = %#v", labels["root"])
	}
	if got["untouched"] != true {
		t.Fatalf("untouched = %#v", got["untouched"])
	}
	// The input map must not be mutated: it may be shared with loopback callers.
	if in["path"] != "/home/u/repo" {
		t.Fatal("Map mutated the input map")
	}
}

func TestMapLoopbackAndNil(t *testing.T) {
	in := map[string]any{"path": "/home/u/repo"}
	got := Map(local(), in)
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("loopback Map = %#v, want %#v", got, in)
	}
	if Map(lan(), nil) != nil {
		t.Fatal("Map(nil) must stay nil for omitempty")
	}
}
