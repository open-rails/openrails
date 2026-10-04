package operator

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
)

// ProvisionMerchant idempotently provisions a merchant at runtime through the
// attached control plane (#738): the live merchant the name resolves to, or a
// new one claiming it, bound to a new merchant permission-group (persona
// merchant, parent root, owner req.OwnerUserID). Safe to re-run.
//
// This is the engine mechanism behind a hosted wrapper's "registration is
// provisioning" flow. Calling it without an attached control plane is a wiring
// error (call Attach/AttachWithOptions first).
func ProvisionMerchant(ctx context.Context, a *app.App, req billing.ProvisionMerchantRequest) (*billing.ProvisionMerchantResult, error) {
	cp := Get(a)
	if cp == nil || cp.Core() == nil {
		return nil, fmt.Errorf("control plane provision: no control plane attached (call Attach first)")
	}
	slug := billing.NormalizeMerchantSlug(req.Slug)
	if err := billing.ValidateMerchantSlug(slug); err != nil {
		return nil, fmt.Errorf("%w: %w", billing.ErrInvalidMerchantSlug, err)
	}
	if err := cp.EnforceMerchantCreationPolicy(ctx, slug, req.OwnerUserID); err != nil {
		return nil, fmt.Errorf("control plane provision: %w", err)
	}
	m, created, err := cp.ProvisionMerchant(ctx, slug, req.OwnerUserID)
	if err != nil {
		return nil, fmt.Errorf("control plane provision %q: %w", slug, err)
	}
	return &billing.ProvisionMerchantResult{MerchantID: m.ID, GroupID: m.PermissionGroupID, Created: created}, nil
}
