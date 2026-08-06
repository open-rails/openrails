package controlplane

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/internal/app"
)

// MerchantRef is a merchant's directory identity as a host sees it: the slug it
// scopes billing to, plus an optional human-facing display name. It answers both
// "which merchants does this customer transact with" (ListMerchantsForSubject,
// openrails-saas #18) and "what are these merchants called" (ListMerchantRefs).
type MerchantRef struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name,omitempty"`
}

// ListMerchantsForSubject returns the active merchants where the AuthKit subject
// holds a customer record (openrails-saas #18) — the "which merchants do I buy
// from" enumeration a hosted customer portal needs, and which no per-merchant,
// RLS-scoped surface can answer. Delegates to the control plane's cross-merchant
// directory read, which goes through the SECURITY DEFINER directory function
// added by migration 0016 (#824 — there is no "privileged pool"; a GUC-less
// base-pool read of customers returns nothing under the production role).
// Calling it without an attached control plane is a wiring error (call
// Attach/AttachWithOptions first).
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
