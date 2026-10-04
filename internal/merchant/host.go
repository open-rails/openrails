package merchant

import "context"

// HostResolver resolves the merchant that owns an inbound request Host header
// (#734) — shared by the Host-based merchant-scoped route resolution and the
// Host-routed webhook mount (pkg/embedded). Browser CORS no longer uses this
// (#765: CORS is a static per-route-tier policy, not sourced from Host/merchant
// resolution). Implementations MUST resolve LIVE per call: OpenRails never
// builds a boot-time host->merchant map, so a
// merchant registered (or reconfigured) on any node resolves immediately on
// every other node/process sharing the same database — no restart, no cache
// refresh required. Returns a zero ID and a non-nil error when host maps to no
// active merchant (unknown/disabled/ambiguous host) — callers MUST fail closed
// on error, never fall back to a default merchant.
type HostResolver func(ctx context.Context, host string) (ID, error)

// hostMerchantCtxKey is the unexported context key for the Host-pinned
// merchant. Deliberately distinct from the general "configured merchant"
// WithID/FromContext key: HTTP Host resolution sets both together, but only
// THIS key lets the JWT-issuer resolution path (internal/controlplane)
// positively detect "a Host resolver ran and pinned merchant X for this
// request" so it can enforce X == the token's own issuer-resolved merchant.
type hostMerchantCtxKey struct{}

// WithHostMerchant pins the merchant a HostResolver resolved from the
// request's Host header. A deployment that never configures Host resolution
// never sets this key, so any check gated on HostMerchant is a pure no-op —
// single-merchant self-hosters see no behavior change without opting in
// (#734).
func WithHostMerchant(ctx context.Context, id ID) context.Context {
	return context.WithValue(ctx, hostMerchantCtxKey{}, id)
}

// HostMerchant returns the Host-pinned merchant set by WithHostMerchant, if
// any.
func HostMerchant(ctx context.Context) (ID, bool) {
	if ctx == nil {
		return ID{}, false
	}
	v := ctx.Value(hostMerchantCtxKey{})
	if v == nil {
		return ID{}, false
	}
	id, ok := v.(ID)
	if !ok || id.IsZero() {
		return ID{}, false
	}
	return id, true
}
