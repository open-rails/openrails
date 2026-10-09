package entitlements

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
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

// ResolveEffectiveTiers resolves each customer's effective tier in tier group
// `group` at `at`; a customer with none is absent.
// Archived products of the group still count: their holders keep them.
func (s *EntitlementService) ResolveEffectiveTiers(ctx context.Context, customers []uuid.UUID, group string, at time.Time) (map[uuid.UUID]EffectiveTier, error) {
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
	rows, err := s.db.Gen(ctx).ResolveEffectiveTiers(ctx, gen.ResolveEffectiveTiersParams{MerchantID: mid.UUID(), CustomerIds: customers, TierGroup: group, At: at})
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]EffectiveTier, len(rows))
	for _, row := range rows {
		out[row.CustomerID] = EffectiveTier{TierGroup: group, Entitlement: row.Entitlement, ProductID: row.ProductID, ProductKey: row.ProductKey,
			ProductDisplayName: row.ProductDisplayName, TierRank: int(row.TierRank)}
	}
	return out, nil
}
