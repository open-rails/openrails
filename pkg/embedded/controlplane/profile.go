package controlplane

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrMerchantNotFound indicates that no active merchant matched the requested
// directory update.
var ErrMerchantNotFound = merchants.ErrMerchantNotFound

// ListMerchantRefs returns the directory identity — slug plus display name — of
// each requested slug, for the slugs that exist. It is the read counterpart of
// SetMerchantDisplayName, and the seam a host needs to label merchant surfaces
// it reaches through a MEMBERSHIP rather than a customer record: AuthKit
// subject-group membership carries only the instance slug, and the customer-side
// enumeration (ListMerchantsForSubject) is scoped to customer records, which a
// merchant's own owner does not hold. Unknown slugs are omitted. This is a
// privileged host seam; callers must authorize the slugs before use.
func ListMerchantRefs(ctx context.Context, a *app.App, slugs []string) ([]MerchantRef, error) {
	cp := Get(a)
	if cp == nil {
		return nil, fmt.Errorf("control plane list merchant refs: no control plane attached (call Attach first)")
	}
	dir, err := merchants.NewDirectoryService(cp.Pool())
	if err != nil {
		return nil, fmt.Errorf("control plane list merchant refs: build merchant directory service: %w", err)
	}
	rows, err := dir.ListDirectoryRefs(ctx, slugs)
	if err != nil {
		return nil, fmt.Errorf("control plane list merchant refs: %w", err)
	}
	out := make([]MerchantRef, 0, len(rows))
	for _, r := range rows {
		out = append(out, MerchantRef{Slug: r.Slug, DisplayName: r.DisplayName})
	}
	return out, nil
}

// SetMerchantDisplayName sets an active merchant's human-readable directory
// name through the attached control plane. Empty names are ignored so hosts can
// safely retry provisioning without clearing a previously stored name. This is
// a privileged host seam; callers must authorize the merchant ID before use.
func SetMerchantDisplayName(ctx context.Context, a *app.App, id merchant.ID, displayName string) error {
	cp := Get(a)
	if cp == nil {
		return fmt.Errorf("control plane set merchant display name: no control plane attached (call Attach first)")
	}
	dir, err := merchants.NewDirectoryService(cp.Pool())
	if err != nil {
		return fmt.Errorf("control plane set merchant display name: build merchant directory service: %w", err)
	}
	if err := dir.SetDisplayName(ctx, id, displayName); err != nil {
		return fmt.Errorf("control plane set merchant display name: %w", err)
	}
	return nil
}
