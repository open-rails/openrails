package billingauth

import (
	"context"

	"github.com/open-rails/openrails/pkg/merchant"
)

// HostPrincipal is the identity an IN-PROCESS transport attaches to the request
// context when an embedding host calls its own engine through the unified SDK
// client (#685). It is deliberately a CONTEXT VALUE, not a header: context
// values cannot arrive on a network request, so a gate that finds one can trust
// it without any shared secret — unforgeable from the wire by construction.
// Permissions are authoritative; the in-process host is trusted for its own
// merchant (same trust stance as billingauth.DelegatedPrincipal, #339).
type HostPrincipal struct {
	MerchantID   merchant.ID
	MerchantSlug string
	Permissions  []string
}

type hostPrincipalCtxKey struct{}

// WithHostPrincipal attaches the in-process host principal to ctx. Only
// in-process callers can reach this; a request arriving over the network can
// never carry it.
func WithHostPrincipal(ctx context.Context, p *HostPrincipal) context.Context {
	return context.WithValue(ctx, hostPrincipalCtxKey{}, p)
}

// HostPrincipalFromContext returns the in-process host principal, if any.
func HostPrincipalFromContext(ctx context.Context) (*HostPrincipal, bool) {
	p, ok := ctx.Value(hostPrincipalCtxKey{}).(*HostPrincipal)
	return p, ok && p != nil
}
