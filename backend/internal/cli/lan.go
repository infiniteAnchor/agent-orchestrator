package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// lanStatusDTO is the CLI-local view of GET/POST /api/v1/mobile/{status,enable,disable,regenerate}.
// It deliberately omits tunnel internals, Cloudflare fields, and securePairing.
type lanStatusDTO struct {
	Enabled       bool   `json:"enabled"`
	HostID        string `json:"hostId"`
	Password      string `json:"password,omitempty"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	TailscaleHost string `json:"tailscaleHost"`
	LanOnly       bool   `json:"lanOnly"`
	Warning       string `json:"warning"`
}

// lanOnlyBody is posted by ao lan enable/regenerate so the daemon skips
// Cloudflare remote-access startup.
type lanOnlyBody struct {
	LanOnly bool `json:"lanOnly"`
}

func newLanCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "lan",
		Short: "Enable, disable, or rotate the opt-in LAN listener password",
		Long: "Control the daemon's opt-in LAN listener over the local loopback API.\n\n" +
			"These commands talk only to /api/v1/mobile/status|enable|disable|regenerate.\n" +
			"Enable and regenerate post lanOnly=true so the daemon does not start the\n" +
			"Cloudflare remote-access connector (and stops one if already running).\n" +
			"They do not configure Tailscale Serve.\n\n" +
			"Enable and regenerate print the opaque host id and connection password as\n" +
			"separate fields so a desktop can pin the host id before sending the secret.",
		Example: `  ao lan status
  ao lan enable
  ao lan regenerate --json
  ao lan disable`,
	}
	cmd.AddCommand(newLanStatusCommand(ctx))
	cmd.AddCommand(newLanEnableCommand(ctx))
	cmd.AddCommand(newLanDisableCommand(ctx))
	cmd.AddCommand(newLanRegenerateCommand(ctx))
	return cmd
}

func newLanStatusCommand(ctx *commandContext) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether the LAN listener is enabled",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := ctx.lanStatus(cmd.Context())
			if err != nil {
				return err
			}
			return writeLanStatus(cmd.OutOrStdout(), st, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output status as JSON")
	return cmd
}

func newLanEnableCommand(ctx *commandContext) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "enable",
		Short: "Enable the LAN listener and mint a connection password",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := ctx.lanEnable(cmd.Context())
			if err != nil {
				return err
			}
			return writeLanStatus(cmd.OutOrStdout(), st, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output result as JSON")
	return cmd
}

func newLanDisableCommand(ctx *commandContext) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "disable",
		Short: "Disable the LAN listener",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := ctx.lanDisable(cmd.Context())
			if err != nil {
				return err
			}
			return writeLanStatus(cmd.OutOrStdout(), st, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output result as JSON")
	return cmd
}

func newLanRegenerateCommand(ctx *commandContext) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "regenerate",
		Short: "Rotate the LAN connection password",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := ctx.lanRegenerate(cmd.Context())
			if err != nil {
				return err
			}
			return writeLanStatus(cmd.OutOrStdout(), st, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output result as JSON")
	return cmd
}

func (c *commandContext) lanStatus(ctx context.Context) (lanStatusDTO, error) {
	var out lanStatusDTO
	if err := c.getJSON(ctx, "mobile/status", &out); err != nil {
		return lanStatusDTO{}, err
	}
	return out, nil
}

func (c *commandContext) lanEnable(ctx context.Context) (lanStatusDTO, error) {
	var out lanStatusDTO
	if err := c.postJSON(ctx, "mobile/enable", lanOnlyBody{LanOnly: true}, &out); err != nil {
		return lanStatusDTO{}, err
	}
	return out, nil
}

func (c *commandContext) lanDisable(ctx context.Context) (lanStatusDTO, error) {
	var out lanStatusDTO
	if err := c.postJSON(ctx, "mobile/disable", struct{}{}, &out); err != nil {
		return lanStatusDTO{}, err
	}
	return out, nil
}

func (c *commandContext) lanRegenerate(ctx context.Context) (lanStatusDTO, error) {
	var out lanStatusDTO
	if err := c.postJSON(ctx, "mobile/regenerate", lanOnlyBody{LanOnly: true}, &out); err != nil {
		return lanStatusDTO{}, err
	}
	return out, nil
}

func writeLanStatus(w io.Writer, st lanStatusDTO, asJSON bool) error {
	if asJSON {
		return writeJSON(w, st)
	}
	state := "disabled"
	if st.Enabled {
		state = "enabled"
	}
	if _, err := fmt.Fprintf(w, "LAN listener: %s\n", state); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  host id: %s\n", st.HostID); err != nil {
		return err
	}
	if st.LanOnly {
		if _, err := fmt.Fprintln(w, "  mode: lan-only (Cloudflare remote access not started)"); err != nil {
			return err
		}
	}
	if st.Host != "" {
		if _, err := fmt.Fprintf(w, "  host: %s\n", st.Host); err != nil {
			return err
		}
	}
	if st.Port != 0 {
		if _, err := fmt.Fprintf(w, "  port: %d\n", st.Port); err != nil {
			return err
		}
	}
	if st.TailscaleHost != "" {
		if _, err := fmt.Fprintf(w, "  tailscale host: %s\n", st.TailscaleHost); err != nil {
			return err
		}
	}
	if st.Password != "" {
		if _, err := fmt.Fprintf(w, "  password: %s\n", st.Password); err != nil {
			return err
		}
	}
	if st.Warning != "" {
		if _, err := fmt.Fprintf(w, "  warning: %s\n", st.Warning); err != nil {
			return err
		}
	}
	if st.Password != "" {
		if _, err := fmt.Fprintln(w, "Pin the host id in the desktop before sending the password."); err != nil {
			return err
		}
	}
	return nil
}
