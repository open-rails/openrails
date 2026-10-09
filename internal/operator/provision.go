package operator

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/controlplane"
)

// ProvisionMerchant idempotently provisions a merchant at runtime through the
// attached control plane (#738): the live merchant the name resolves to, or a
// new one claiming it, bound to a new merchant permission-group (persona
// merchant, parent root, owner req.OwnerUserID). Safe to re-run.
//
// This is the mechanism behind a hosted wrapper's "registration is
// provisioning" flow.
func ProvisionMerchant(ctx context.Context, cp *controlplane.ControlPlane, req billing.ProvisionMerchantParams) (*billing.ProvisionMerchantResult, error) {
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
	if err := cp.SetMerchantDisplayName(ctx, m.ID, req.DisplayName); err != nil {
		return nil, fmt.Errorf("control plane provision %q: display name: %w", slug, err)
	}
	return &billing.ProvisionMerchantResult{MerchantID: m.ID, GroupID: m.PermissionGroupID, Created: created}, nil
}
