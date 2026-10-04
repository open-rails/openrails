package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/billing"
)

// -------------------------------- Billing Status --------------------------------

// ResolveEffectiveTier returns THE effective tier for a user within one tier
// group (or#912): the highest-ranked non-archived product whose declared
// entitlements intersect the user's active entitlement windows now.
// Overlapping active entitlements (mid-upgrade) deterministically resolve to
// the highest tier_rank — never an error. Returns (nil, nil) when the user
// holds no active entitlement in the group; the host applies its own default.
func (s *Service) ResolveEffectiveTier(ctx context.Context, userID, group string) (*EffectiveTier, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}
	if s.rt.EntitlementService == nil {
		return nil, fmt.Errorf("entitlement service unavailable")
	}
	tier, err := s.rt.EntitlementService.ResolveEffectiveTier(ctx, userID, group, s.now().UTC())
	if err != nil {
		return nil, err
	}
	if tier == nil {
		return nil, nil
	}
	return &EffectiveTier{
		Group:       tier.TierGroup,
		Entitlement: tier.Entitlement,
		DisplayName: tier.ProductDisplayName,
		TierRank:    tier.TierRank,
		ProductID:   billing.ProductID(tier.ProductID).String(),
		ProductKey:  tier.ProductKey,
	}, nil
}
