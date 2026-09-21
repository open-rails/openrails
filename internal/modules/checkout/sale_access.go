package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/pkg/merchant"
)

type acceptedAccessWindow struct {
	start time.Time
	end   *time.Time
}

func sameAccessEnd(a, b *time.Time) bool {
	return a == nil && b == nil || a != nil && b != nil && a.Equal(*b)
}

// applyAcceptedPurchaseAccess uses the same grant ledger as ordinary purchases,
// with exact accepted intervals and validation of original source windows.
// Ordinary indefinite entitlement insertion can advance a historical NotBefore
// to now; a delayed accepted purchase must retain its historical start.
func (s *CheckoutPurchaseService) applyAcceptedPurchaseAccess(ctx context.Context, user string, product, payment uuid.UUID, spec map[string]*int, duration *int, accepted time.Time, coverage *CoverageInfo) error {
	if s.transactionDB == nil || s.transactionDB.Pool() != nil {
		return errors.New("accepted access requires purchase transaction")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	customer, err := uuid.Parse(user)
	if err != nil {
		return err
	}
	start := accepted
	if coverage != nil && coverage.EndDate != nil {
		start = *coverage.EndDate
	}
	names := make([]string, 0, len(spec))
	wanted := map[string]acceptedAccessWindow{}
	for name, hours := range spec {
		names = append(names, name)
		var end *time.Time
		if duration != nil && *duration > 0 {
			v := start.Add(time.Duration(*duration) * time.Hour)
			end = &v
		} else if duration == nil && hours != nil && *hours > 0 {
			v := start.Add(time.Duration(*hours) * time.Hour)
			end = &v
		}
		wanted[name] = acceptedAccessWindow{start, end}
	}
	sort.Strings(names)
	for _, name := range names {
		if err := entitlements.LockEntitlementTimeline(ctx, s.transactionDB.Qx(ctx), user, name); err != nil {
			return err
		}
	}
	q := s.transactionDB.Gen(ctx)
	original, err := q.ListOriginalPurchaseGrants(ctx, gen.ListOriginalPurchaseGrantsParams{MerchantID: mid.UUID(), PaymentID: payment, RowLimit: int32(len(names) + 2)})
	if err != nil {
		return err
	}
	var ownership *gen.OpenrailsGrant
	existing := map[string]gen.OpenrailsGrant{}
	var ownershipEnd *time.Time
	if duration != nil && *duration > 0 {
		v := accepted.Add(time.Duration(*duration) * time.Hour)
		ownershipEnd = &v
	}
	for _, g := range original {
		if g.CustomerID != customer || g.PaymentID != nil && *g.PaymentID != payment || g.ProductID != nil && *g.ProductID != product {
			return errors.New("original grant belongs to another accepted purchase")
		}
		switch g.Kind {
		case string(grants.Ownership):
			if ownership != nil || g.ProductID == nil || !g.StartsAt.Equal(accepted) || !sameAccessEnd(g.EndsAt, ownershipEnd) {
				return errors.New("original ownership window contradicts accepted purchase")
			}
			copied := g
			ownership = &copied
		case string(grants.Entitlement):
			var encoded grants.Spec
			if err := json.Unmarshal(g.SpecSnapshot, &encoded); err != nil || len(encoded.Entitlements) == 0 || encoded.Deposit != nil {
				return errors.New("original entitlement spec contradicts accepted purchase")
			}
			for _, name := range encoded.Entitlements {
				window, ok := wanted[name]
				if _, duplicate := existing[name]; !ok || duplicate || !g.StartsAt.Equal(window.start) || !sameAccessEnd(g.EndsAt, window.end) {
					return errors.New("original entitlement window contradicts accepted purchase")
				}
				existing[name] = g
			}
		default:
			return errors.New("purchase has an unsupported original grant")
		}
	}
	ledger := grants.New(q, mid.UUID())
	ledger.SetClock(s.now)
	materialized := map[uuid.UUID]bool{}
	for _, name := range names {
		g, found := existing[name]
		if !found {
			window := wanted[name]
			g, err = ledger.Grant(ctx, grants.GrantInput{Customer: customer, Kind: grants.Entitlement, Source: grants.Purchase, SourceID: payment.String(), Payment: &payment, Spec: &grants.Spec{Entitlements: []string{name}}, StartsAt: window.start, EndsAt: window.end})
			if err != nil {
				return err
			}
		}
		if !materialized[g.ID] {
			if err := ledger.MaterializeGrant(ctx, g); err != nil {
				return err
			}
			materialized[g.ID] = true
		}
	}
	if ownership == nil {
		_, err = ledger.Grant(ctx, grants.GrantInput{Customer: customer, Product: &product, Kind: grants.Ownership, Source: grants.Purchase, SourceID: payment.String(), Payment: &payment, StartsAt: accepted, EndsAt: ownershipEnd})
	}
	return err
}
