package operator

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/auth/policy"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrMerchantNotFound indicates that no active merchant matched the requested
// directory update.
var ErrMerchantNotFound = merchants.ErrMerchantNotFound
var ErrPermissionRequired = policy.ErrPermissionRequired
var ErrMerchantUnresolved = policy.ErrMerchantUnresolved

// ListMerchantRefs returns the directory identity — slug plus display name — of
// each requested slug, for the slugs that exist. It is the read counterpart of
// SetMerchantDisplayName. Unknown slugs are omitted. This is a privileged host
// seam; callers must authorize the slugs before use.
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
		out = append(out, MerchantRef{ID: r.ID, Slug: r.Slug, DisplayName: r.DisplayName})
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

// RenameMerchant renames an active merchant as the operator: no reserved-name
// or rename-interval check, and the former name forwards to it under the site
// naming policy. This is a privileged host seam; callers must authorize it.
func RenameMerchant(ctx context.Context, a *app.App, id merchant.ID, name string) error {
	cp := Get(a)
	if cp == nil {
		return fmt.Errorf("control plane rename merchant: no control plane attached (call Attach first)")
	}
	if _, err := cp.RenameMerchant(ctx, id, name, "", true); err != nil {
		return fmt.Errorf("control plane rename merchant: %w", err)
	}
	return nil
}
