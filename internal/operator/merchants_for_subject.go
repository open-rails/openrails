package operator

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
)

// ListMerchantsForSubject returns the active merchants where the AuthKit subject
// holds a customer record — the "which merchants do I buy
// from" enumeration a hosted customer portal needs, and which no per-merchant
// surface can answer. Delegates to the control plane's cross-merchant
// directory read.
// Calling it without an attached control plane is a wiring error (call
// Attach/AttachWithOptions first).
func ListMerchantsForSubject(ctx context.Context, a *app.App, subject string) ([]billing.MerchantRef, error) {
	cp := Get(a)
	if cp == nil {
		return nil, fmt.Errorf("control plane: no control plane attached (call Attach first)")
	}
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
