package entitlements

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// EffectiveTier is the single winning tier for a customer within one tier
// group (or#912): the highest-ranked product of the group granting a key the
// customer holds. Entitlement and ProductKey are IMMUTABLE identifiers;
// ProductDisplayName is for display only.
type EffectiveTier struct {
	TierGroup          string
	Entitlement        string
	ProductID          uuid.UUID
	ProductKey         string
	ProductDisplayName string
	TierRank           int
}

// ResolveEffectiveTier resolves the effective tier for a user (self-service
// identity) in tier group `group` at `at`. (nil, nil) is "no tier".
func (s *EntitlementService) ResolveEffectiveTier(ctx context.Context, userID, group string, at time.Time) (*EffectiveTier, error) {
	customer, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	return s.ResolveEffectiveTierByCustomer(ctx, customer, group, at)
}

// ResolveEffectiveTierByCustomer is ResolveEffectiveTier keyed by customer.
// Archived products of the group still count: their holders keep them.
func (s *EntitlementService) ResolveEffectiveTierByCustomer(ctx context.Context, customer uuid.UUID, group string, at time.Time) (*EffectiveTier, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("entitlement service not initialized")
	}
	group = strings.TrimSpace(group)
	if group == "" {
		return nil, fmt.Errorf("tier group is required")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := s.db.Gen(ctx).ResolveEffectiveTier(ctx, gen.ResolveEffectiveTierParams{MerchantID: mid.UUID(), CustomerID: customer, TierGroup: group, At: at})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &EffectiveTier{TierGroup: group, Entitlement: row.Entitlement, ProductID: row.ProductID, ProductKey: row.ProductKey,
		ProductDisplayName: row.ProductDisplayName, TierRank: int(row.TierRank)}, nil
}
