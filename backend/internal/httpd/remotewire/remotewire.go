// Package remotewire produces LAN-safe projections of host-local wire values.
//
// The daemon's loopback listener serves the local desktop and CLI and may tell
// them where things live on this machine. The opt-in LAN listener serves remote
// clients (a phone, or a desktop connected to a headless server) and must not
// disclose the host's filesystem layout. Controllers call these helpers with the
// request context; loopback requests pass through untouched, so local behavior
// is unchanged.
package remotewire

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/reqctx"
)

// pathPlaceholder replaces an absolute path embedded in free text.
const pathPlaceholder = "<path>"

// Path returns s unchanged on loopback and the empty string on LAN. Use it for
// structured fields that are host-local paths by contract (project roots, repo
// paths, resolved executable destinations).
func Path(ctx context.Context, s string) string {
	if reqctx.IsLAN(ctx) {
		return ""
	}
	return s
}

// Text returns s unchanged on loopback. On LAN it removes host-absolute
// filesystem paths: a value that is only a path becomes "", and a path embedded
// in longer diagnostic text is replaced with "<path>". Free-text fields (probe
// details, installer output, provider warnings, error messages) use it so a
// remote client still learns what failed without learning where this host keeps
// its files.
func Text(ctx context.Context, s string) string {
	if !reqctx.IsLAN(ctx) || s == "" {
		return s
	}
	trimmed := strings.TrimSpace(s)
	// A value that is only a path becomes empty. "Only a path" means no
	// whitespace, no leading "//" (a comment or protocol-relative prefix), and
	// no newline: anything longer is prose that the redactor handles token by
	// token so the message survives.
	if isHostAbsolute(trimmed) && !strings.HasPrefix(trimmed, "//") && !strings.ContainsAny(trimmed, " \t\n\r") {
		return ""
	}
	return redactHostPaths(s)
}

// Map returns a copy of in with every string value (recursively) passed through
// Text. It returns nil for a nil map so omitempty behavior is preserved.
func Map(ctx context.Context, in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = Value(ctx, value)
	}
	return out
}

// Value applies Text to every string reachable through the common JSON shapes
// (map, slice, string). Values of any other type are returned unchanged.
func Value(ctx context.Context, v any) any {
	switch typed := v.(type) {
	case string:
		return Text(ctx, typed)
	case map[string]any:
		return Map(ctx, typed)
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = Value(ctx, item)
		}
		return out
	case []string:
		out := make([]string, len(typed))
		for i, item := range typed {
			out[i] = Text(ctx, item)
		}
		return out
	case map[string]string:
		out := make(map[string]string, len(typed))
		for key, item := range typed {
			out[key] = Text(ctx, item)
		}
		return out
	default:
		return v
	}
}

// isHostAbsolute reports whether s is a whole host-absolute path, including the
// Windows drive and home-relative forms filepath.IsAbs misses on another OS.
func isHostAbsolute(s string) bool {
	if s == "" {
		return false
	}
	if filepath.IsAbs(s) {
		return true
	}
	if strings.HasPrefix(s, "~/") || strings.HasPrefix(s, `~\`) {
		return true
	}
	return windowsDrivePath.MatchString(s)
}

// windowsDrivePath matches "C:\..." and "C:/...".
var windowsDrivePath = regexp.MustCompile(`(?i)^[A-Za-z]:[\\/]`)

// boundaryRunes are the characters that may precede an absolute path in free
// text. ":" is deliberately absent: it is what precedes the "//" of a URL, and
// treating it as a boundary would turn "https://host/a/b" into a path.
const boundaryClass = `\s"'` + "`" + `(\[{=,;`

// hostPathPattern matches one host-absolute path token, including its optional
// leading boundary character so the boundary survives replacement. The
// alternation covers POSIX roots, Windows drive paths, file:// URLs, UNC
// shares, and home-relative paths. The body stops at whitespace, quotes,
// closing brackets, and list punctuation so surrounding prose is preserved.
var hostPathPattern = regexp.MustCompile(`(?:^|[` + boundaryClass + `])(?:[A-Za-z]:[\\/]|file://|\\\\|~[\\/]|/)[^\s"'` + "`" + `)\]},;>]+`)

// quotedHostPathPattern matches a host-absolute path wrapped in quotes. Such a
// path may contain spaces (a macOS "My Projects" folder), which the unquoted
// pattern cannot span, so it is redacted as a whole token first.
var quotedHostPathPattern = regexp.MustCompile(`(?:"(?:[A-Za-z]:[\\/]|file://|\\\\|~[\\/]|/)[^"]*"|'(?:[A-Za-z]:[\\/]|file://|\\\\|~[\\/]|/)[^']*')`)

// trailingPunctuation is returned to the text after a redacted path so a
// sentence keeps its period.
const trailingPunctuation = ".,:!?"

func redactHostPaths(s string) string {
	s = quotedHostPathPattern.ReplaceAllStringFunc(s, func(match string) string {
		quote := match[:1]
		return quote + pathPlaceholder + quote
	})
	return hostPathPattern.ReplaceAllStringFunc(s, func(match string) string {
		lead, rest := splitBoundary(match)
		// A bare "//" is a comment or protocol-relative prefix, not a path.
		if strings.HasPrefix(rest, "//") && !strings.HasPrefix(rest, "file://") {
			return match
		}
		trimmed := strings.TrimRight(rest, trailingPunctuation)
		suffix := rest[len(trimmed):]
		return lead + pathPlaceholder + suffix
	})
}

// splitBoundary separates a leading boundary character from the path token. An
// empty lead means the match began at the start of the string.
func splitBoundary(match string) (string, string) {
	first, size := utf8.DecodeRuneInString(match)
	if size > 0 && isBoundaryRune(first) {
		return match[:size], match[size:]
	}
	return "", match
}

func isBoundaryRune(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '\f', '\v', '"', '\'', '`', '(', '[', '{', '=', ',', ';':
		return true
	default:
		return false
	}
}
