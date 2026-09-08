// Package reqctx carries per-request metadata that HTTP handlers need without
// importing the full httpd package (controllers cannot import httpd).
package reqctx

import "context"

type lanKey struct{}

// WithLAN marks ctx as a request that arrived on the opt-in LAN listener.
// Loopback requests never set this.
func WithLAN(ctx context.Context) context.Context {
	return context.WithValue(ctx, lanKey{}, true)
}

// IsLAN reports whether ctx was marked by WithLAN.
func IsLAN(ctx context.Context) bool {
	v, _ := ctx.Value(lanKey{}).(bool)
	return v
}
