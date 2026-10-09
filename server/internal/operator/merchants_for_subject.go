package operator

import (
	"context"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/server/internal/controlplane"
)

// ListMerchantsForSubject returns the active merchants where the AuthKit subject
// holds a customer record — the "which merchants do I buy
// from" enumeration a hosted customer portal needs, and which no per-merchant
// surface can answer. Delegates to the control plane's cross-merchant
// directory read.
func ListMerchantsForSubject(ctx context.Context, cp *controlplane.ControlPlane, subject string) ([]billing.MerchantRef, error) {
	rows, err := cp.ListMerchantsForSubject(ctx, subject)
	if err != nil {
		return nil, err
	}
	out := make([]billing.MerchantRef, 0, len(rows))
	for _, r := range rows {
		out = append(out, billing.MerchantRef{ID: r.ID, Slug: r.Slug, DisplayName: r.DisplayName})
	}
	return out, nil
}
