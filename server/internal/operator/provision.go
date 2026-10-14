package operator

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/server/internal/controlplane"
)

// ProvisionMerchant idempotently provisions a merchant: the live merchant the
// name resolves to, or a new one claiming it, bound to a new merchant
// permission-group (parent root, owner req.OwnerUserID). It backs a hosted
// product's "registration is provisioning" flow.
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
	// A user's claim names only the merchant it creates.
	if created || req.OwnerUserID == "" {
		if err := cp.SetMerchantDisplayName(ctx, m.ID, req.DisplayName); err != nil {
			return nil, fmt.Errorf("control plane provision %q: display name: %w", slug, err)
		}
	}
	return &billing.ProvisionMerchantResult{MerchantID: m.ID, GroupID: m.PermissionGroupID, Created: created}, nil
}
