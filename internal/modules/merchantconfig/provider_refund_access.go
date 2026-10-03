package merchantconfig

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
)

// NormalizeProviderRefundAccess validates a provider_refund_access value; ""
// means the default.
func NormalizeProviderRefundAccess(v string) (string, error) {
	switch v {
	case "", billing.ProviderRefundRevokeOnFull, billing.ProviderRefundRevokeOnAny, billing.ProviderRefundKeep:
		return v, nil
	}
	return "", fmt.Errorf("provider_refund_access must be %q, %q or %q", billing.ProviderRefundRevokeOnFull, billing.ProviderRefundRevokeOnAny, billing.ProviderRefundKeep)
}

// ProviderRefundRevokes applies the merchant's provider_refund_access policy
// to a refund made at the provider (its dashboard or API, not through
// OpenRails): whether the refunded charge's access ends, given everything
// refunded so far against what was captured. The default revokes on a full
// refund.
func ProviderRefundRevokes(ctx context.Context, d *db.DB, refunded, captured int64) (bool, error) {
	cfg, _, err := NewStore(d).Get(ctx)
	if err != nil {
		return false, fmt.Errorf("load provider refund policy: %w", err)
	}
	switch cfg.ProviderRefundAccess {
	case billing.ProviderRefundKeep:
		return false, nil
	case billing.ProviderRefundRevokeOnAny:
		return refunded > 0, nil
	default:
		return captured > 0 && refunded >= captured, nil
	}
}
