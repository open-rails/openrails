package money

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ReconcileReport is the alert-only output of Reconcile. With balance + held
// DERIVED from the source of truth (money_blocks + active holds/windows, #491)
// there is no cache to drift against — the only remaining check is orphaned
// holds (active past expiry that the HoldExpiryWorker should have released).
type ReconcileReport struct {
	GeneratedAt   time.Time      `json:"generated_at"`
	OrphanedHolds []OrphanedHold `json:"orphaned_holds"`
}

// OrphanedHold is an active hold that has passed its expiry and should have been
// released by HoldExpiryWorker.
type OrphanedHold struct {
	ID         uuid.UUID  `json:"id"`
	CustomerID uuid.UUID  `json:"customer_id"`
	Amount     int64      `json:"authorized_amount"`
	ExpiresAt  *time.Time `json:"expires_at"`
}

// Reconcile runs the alert-only consistency check and returns a report. It never
// mutates state (issue #243, alert-first).
func (s *MoneyService) Reconcile(ctx context.Context) (ReconcileReport, error) {
	if s == nil || s.db == nil {
		return ReconcileReport{}, fmt.Errorf("money service not initialized")
	}
	rep := ReconcileReport{GeneratedAt: s.now()}
	orphans, err := s.FindOrphanedExpiredHolds(ctx)
	if err != nil {
		return rep, err
	}
	rep.OrphanedHolds = orphans
	return rep, nil
}

// FindOrphanedExpiredHolds returns holds still 'active' whose expiry has passed.
func (s *MoneyService) FindOrphanedExpiredHolds(ctx context.Context) ([]OrphanedHold, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tenantID := tid.UUID()
	now := s.now()
	holds, err := s.db.Gen(ctx).ListOrphanedExpiredMoneyHolds(ctx, gen.ListOrphanedExpiredMoneyHoldsParams{
		MerchantID: tenantID, Now: now,
	})
	if err != nil {
		return nil, err
	}
	out := make([]OrphanedHold, 0, len(holds))
	for i := range holds {
		var amt int64
		if holds[i].AuthorizedAmount != nil {
			amt = *holds[i].AuthorizedAmount
		}
		out = append(out, OrphanedHold{ID: holds[i].ID, CustomerID: holds[i].CustomerID, Amount: amt, ExpiresAt: holds[i].ExpiresAt})
	}
	return out, nil
}
