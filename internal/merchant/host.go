package merchant

import (
	"context"

	"github.com/open-rails/openrails/billing"
)

// HostResolver resolves the merchant that owns a request's Host header. It
// resolves live per call (no boot-time map), so a merchant registered on any
// node resolves at once everywhere. An unknown, disabled or ambiguous host is
// a zero id and an error; callers fail closed, never falling back to a
// default merchant.
type HostResolver func(ctx context.Context, host string) (billing.MerchantID, error)

// hostMerchantCtxKey is distinct from WithID's key: Host resolution sets both,
// but only this one lets the token path (server/internal/controlplane) see
// that a Host pinned merchant X and require the token's merchant to be X.
type hostMerchantCtxKey struct{}

// WithHostMerchant pins the merchant a HostResolver resolved from the Host
// header. Without Host resolution the key is never set, so checks on
// HostMerchant are no-ops.
func WithHostMerchant(ctx context.Context, id billing.MerchantID) context.Context {
	return context.WithValue(ctx, hostMerchantCtxKey{}, id)
}

// HostMerchant returns the Host-pinned merchant set by WithHostMerchant, if
// any.
func HostMerchant(ctx context.Context) (billing.MerchantID, bool) {
	if ctx == nil {
		return billing.MerchantID{}, false
	}
	id, ok := ctx.Value(hostMerchantCtxKey{}).(billing.MerchantID)
	if !ok || id.IsZero() {
		return billing.MerchantID{}, false
	}
	return id, true
}
