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
	"github.com/open-rails/openrails/pkg/merchant"
)

// EffectiveTier is the single winning tier for a customer within one tier
// group (or#912): the highest-ranked non-archived product whose declared
// entitlements intersect the customer's active entitlement windows.
// Entitlement and ProductKey are IMMUTABLE identifiers (policy documents and
// token claims key on Entitlement); ProductDisplayName is mutable and for
// display only.
type EffectiveTier struct {
	TierGroup          string
	Entitlement        string
	ProductID          uuid.UUID
	ProductKey         string
	ProductDisplayName string
	TierRank           int
}

// ResolveEffectiveTier resolves the effective tier for a user (self-service
// identity) in tier group `group` at `at`. Returns (nil, nil) when the
// customer holds no active entitlement declared by any product in the group —
// "no tier" is a normal answer, never an error. Overlapping active
// entitlements (mid-upgrade) deterministically resolve to the highest
// tier_rank (ties: product key ASC, entitlement ASC).
func (s *EntitlementService) ResolveEffectiveTier(ctx context.Context, userID, group string, at time.Time) (*EffectiveTier, error) {
	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	return s.ResolveEffectiveTierByCustomer(ctx, tsid, group, at)
}

// ResolveEffectiveTierByCustomer is ResolveEffectiveTier keyed by the payable
// customer subject.
func (s *EntitlementService) ResolveEffectiveTierByCustomer(ctx context.Context, tenantSubjectID uuid.UUID, group string, at time.Time) (*EffectiveTier, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("entitlement service not initialized")
	}
	group = strings.TrimSpace(group)
	if group == "" {
		return nil, fmt.Errorf("tier group is required")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := s.db.Gen(ctx).ResolveEffectiveTier(ctx, gen.ResolveEffectiveTierParams{
		MerchantID: tid.UUID(),
		CustomerID: tenantSubjectID,
		TierGroup:  group,
		At:         at,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &EffectiveTier{
		TierGroup:          group,
		Entitlement:        row.Entitlement,
		ProductID:          row.ProductID,
		ProductKey:         row.ProductKey,
		ProductDisplayName: row.ProductDisplayName,
		TierRank:           int(row.TierRank),
	}, nil
}
