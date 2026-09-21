package checkout

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/pkg/merchant"
)

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
	wanted, ownershipWindow := grants.PurchaseWindows(spec, duration, accepted, start)
	names := make([]string, 0, len(wanted))
	for name := range wanted {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := entitlements.LockEntitlementTimeline(ctx, s.transactionDB.Qx(ctx), user, name); err != nil {
			return err
		}
	}
	q := s.transactionDB.Gen(ctx)
	limit, err := safecast.Convert[int32](len(names) + 2)
	if err != nil {
		return err
	}
	original, err := q.ListOriginalPurchaseGrants(ctx, gen.ListOriginalPurchaseGrantsParams{MerchantID: mid.UUID(), PaymentID: payment, RowLimit: limit})
	if err != nil {
		return err
	}
	history, err := grants.ValidatePurchaseHistory(mid.UUID(), customer, product, payment, wanted, ownershipWindow, original)
	if err != nil {
		return err
	}
	ledger := grants.New(q, mid.UUID())
	ledger.SetClock(s.now)
	materialized := map[uuid.UUID]bool{}
	for _, name := range names {
		g, found := history.Entitlements[name]
		if !found {
			window := wanted[name]
			g, err = ledger.Grant(ctx, grants.GrantInput{Customer: customer, Kind: grants.Entitlement, Source: grants.Purchase, SourceID: payment.String(), Payment: &payment, Spec: &grants.Spec{Entitlements: []string{name}}, StartsAt: window.Start, EndsAt: window.End})
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
	if history.Ownership == nil {
		_, err = ledger.Grant(ctx, grants.GrantInput{Customer: customer, Product: &product, Kind: grants.Ownership, Source: grants.Purchase, SourceID: payment.String(), Payment: &payment, StartsAt: accepted, EndsAt: ownershipWindow.End})
	}
	return err
}
