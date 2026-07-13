package controlplane

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/internal/app"
)

// MerchantRef identifies a merchant a customer transacts with (openrails-saas
// #18): the slug the hosted portal scopes billing to, plus an optional
// human-facing display name.
type MerchantRef struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name,omitempty"`
}

// ListMerchantsForSubject returns the active merchants where the AuthKit subject
// holds a customer record (openrails-saas #18) — the "which merchants do I buy
// from" enumeration a hosted customer portal needs, and which no per-merchant,
// RLS-scoped surface can answer. Delegates to the control plane's cross-merchant
// directory read (privileged, non-RLS pool). Calling it without an attached
// control plane is a wiring error (call Attach/AttachWithOptions first).
func ListMerchantsForSubject(ctx context.Context, a *app.App, subject string) ([]MerchantRef, error) {
	cp := Get(a)
	if cp == nil {
		return nil, fmt.Errorf("control plane: no control plane attached (call Attach first)")
	}
	rows, err := cp.ListMerchantsForSubject(ctx, subject)
	if err != nil {
		return nil, err
	}
	out := make([]MerchantRef, 0, len(rows))
	for _, r := range rows {
		out = append(out, MerchantRef{Slug: r.Slug, DisplayName: r.DisplayName})
	}
	return out, nil
}
