package httpd

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/terminal"
)

// terminalMuxReadLimit caps a single inbound frame. Client→server frames are small
// (keystrokes, resize, control), so a generous 1 MiB is ample headroom while
// still bounding memory per message.
const terminalMuxReadLimit = 1 << 20

// mountTerminalMux registers the long-lived terminal-multiplexing WebSocket at /mux. It
// is intentionally outside the per-request Timeout middleware (the connection is
// long-lived). When mgr is nil the route is not mounted — the daemon simply has
// no terminal surface yet.
//
// allowedOrigins is the same CORS allowlist used by the HTTP stack. Browser
// clients that present Origin must match it (or a loopback origin); native /
// Electron-main clients that omit Origin remain allowed.
func mountTerminalMux(r chi.Router, mgr *terminal.Manager, log *slog.Logger, allowedOrigins []string) {
	if mgr == nil {
		return
	}
	r.Get("/mux", terminalMuxHandler(mgr, log, allowedOrigins))
}

// terminalMuxHandler upgrades the request to a WebSocket and hands the connection to the
// terminal manager. httpd owns only the upgrade and the transport adaptation;
// all stream logic lives in internal/terminal.
func terminalMuxHandler(mgr *terminal.Manager, log *slog.Logger, allowedOrigins []string) http.HandlerFunc {
	allowed := exactAllowedOrigins(allowedOrigins)
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if !muxOriginAllowed(origin, allowed) {
			envelope.WriteAPIError(w, r, http.StatusForbidden, "forbidden", "ORIGIN_FORBIDDEN",
				"Origin is not allowed to access the terminal mux", nil)
			return
		}
		// Origin has already been checked against the CORS allowlist / loopback
		// rule (or was absent for a native client). coder/websocket's built-in
		// check only allows same-host origins, which rejects the Electron
		// app://renderer origin connecting to 127.0.0.1, so Accept skips it.
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			log.Warn("terminal mux: websocket upgrade failed", "err", err)
			return
		}
		c.SetReadLimit(terminalMuxReadLimit)
		mgr.Serve(r.Context(), &terminalMuxConn{c: c})
	}
}

// muxOriginAllowed reports whether a WebSocket Origin may upgrade /mux.
// Empty Origin is allowed (CLI, Electron main, other native clients). Present
// origins must be exact allowlist members or loopback-served content — the
// same boundary corsMiddleware enforces for HTTP.
func muxOriginAllowed(origin string, allowed map[string]struct{}) bool {
	if origin == "" {
		return true
	}
	if _, ok := allowed[origin]; ok {
		return true
	}
	return isLoopbackOrigin(origin)
}

// terminalMuxConn adapts a coder/websocket connection to terminal.wsConn. JSON framing
// uses wsjson (text messages); Ping is a control frame; Close sends a normal
// closure.
type terminalMuxConn struct{ c *websocket.Conn }

func (a *terminalMuxConn) ReadJSON(ctx context.Context, v any) error { return wsjson.Read(ctx, a.c, v) }
func (a *terminalMuxConn) WriteJSON(ctx context.Context, v any) error {
	return wsjson.Write(ctx, a.c, v)
}
func (a *terminalMuxConn) Ping(ctx context.Context) error { return a.c.Ping(ctx) }
func (a *terminalMuxConn) Close(reason string) error {
	return a.c.Close(websocket.StatusNormalClosure, reason)
}
