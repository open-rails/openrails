package checkout

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/grants"
)

// applyAcceptedPurchaseAccess grants an accepted purchase's product for its
// exact accepted window: from its accepted start, after any coverage it stacked
// on, for the price's duration. A delayed settlement keeps that historical
// start; a replay returns the recorded grant.
func (s *CheckoutPurchaseService) applyAcceptedPurchaseAccess(ctx context.Context, user string, product, payment uuid.UUID, duration *int, accepted time.Time, coverage *CoverageInfo) error {
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
	window := grants.AccessWindow(duration, start)
	if err := entitlements.LockAccessTimeline(ctx, s.transactionDB.Qx(ctx), user, product); err != nil {
		return err
	}
	q := s.transactionDB.Gen(ctx)
	original, err := q.ListOriginalPurchaseGrants(ctx, gen.ListOriginalPurchaseGrantsParams{MerchantID: mid.UUID(), PaymentID: payment, RowLimit: 10005})
	if err != nil {
		return err
	}
	access := original[:0]
	for _, event := range original {
		// Purchased credit has its own immutable promise validation and source
		// lot; it shares the payment but is not an access window.
		if event.Kind != string(grants.Credit) {
			access = append(access, event)
		}
	}
	recorded, err := grants.RecordedPurchaseAccess(mid.UUID(), customer, product, payment, access)
	if err != nil {
		return err
	}
	ledger := grants.New(q, mid.UUID())
	ledger.SetClock(s.now)
	if recorded != nil {
		if !grants.Migrated(*recorded) && !grants.SameWindow(*recorded, window) && !stackedWindow(*recorded, window) {
			return errors.New("original access window contradicts accepted purchase")
		}
		return ledger.MaterializeGrant(ctx, *recorded)
	}
	// Purchases of one timed product settle in turn under the timeline lock:
	// a window frozen at admission starts after whatever an earlier
	// settlement granted meanwhile, so two purchases never pay for one window.
	if window.End != nil {
		tail, err := entitlements.GetAccessTimelineTailEnd(ctx, s.transactionDB.Qx(ctx), customer, product)
		if err != nil {
			return err
		}
		if tail != nil && tail.After(window.Start) {
			window = grants.AccessWindow(duration, *tail)
		}
	}
	g, _, err := ledger.GrantAccessOnce(ctx, grants.GrantInput{
		Customer: customer, Product: &product, Kind: grants.Access, Source: grants.Purchase, SourceID: payment.String(), Payment: &payment,
		StartsAt: window.Start, EndsAt: window.End,
	})
	if err != nil {
		return err
	}
	return ledger.MaterializeGrant(ctx, g)
}

// stackedWindow: a recorded grant of the accepted duration that starts after
// the accepted window, because settlement stacked it on an earlier purchase.
func stackedWindow(g gen.BillingGrant, w grants.PurchaseWindow) bool {
	return w.End != nil && g.EndsAt != nil && g.StartsAt.After(w.Start) && g.EndsAt.Sub(g.StartsAt) == w.End.Sub(w.Start)
}
