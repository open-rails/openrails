// Package requestauth carries the in-process host principal: a context value
// the Go client attaches, never a header.
package requestauth

import (
	"context"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
)

// HostPrincipal is the identity the in-process transport attaches when an
// embedding host calls its own engine through the Go client. A context value,
// never a header, so it cannot arrive over the network and needs no shared
// secret. It is bound to the merchant of the handle that made the call.
// Invoker is the person the host named for the call; nil is the host's own
// work.
type HostPrincipal struct {
	MerchantID billing.MerchantID
	Invoker    *billingauth.Invoker
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
