package controllers

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
)

// RemoteDesktopContractVersion is the remote-desktop control-plane contract
// version. Bump when authenticated remote HTTP/SSE/mux expectations change in
// a way clients must negotiate. Independent of MobileAPIVersion.
const RemoteDesktopContractVersion = 1

// CapabilitiesController serves authenticated server capability discovery for
// remote desktop clients. This is intentionally separate from the
// unauthenticated identity probe (ADR 0003): capability details must not ride
// on GET /api/v1/identity.
type CapabilitiesController struct{}

// Register mounts the capabilities route on the supplied router.
func (c *CapabilitiesController) Register(r chi.Router) {
	r.Get("/capabilities", c.capabilities)
}

func (c *CapabilitiesController) capabilities(w http.ResponseWriter, _ *http.Request) {
	envelope.WriteJSON(w, http.StatusOK, CapabilitiesResponse{
		APIVersion: MobileAPIVersion,
		RemoteDesktop: RemoteDesktopCapabilities{
			Supported:       true,
			ContractVersion: RemoteDesktopContractVersion,
			Transports:      []string{"http", "sse", "mux"},
			Auth:            "bearer-password",
		},
	})
}
