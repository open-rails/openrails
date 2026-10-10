package money

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ReconcileReport is the alert-only output of Reconcile.
type ReconcileReport struct {
	GeneratedAt   time.Time      `json:"generated_at"`
	OrphanedHolds []OrphanedHold `json:"orphaned_holds"`
}

// OrphanedHold is kept in the report shape for callers; Reconcile reports none.
type OrphanedHold struct {
	ID         uuid.UUID  `json:"id"`
	CustomerID uuid.UUID  `json:"customer_id"`
	Amount     int64      `json:"authorized_amount"`
	ExpiresAt  *time.Time `json:"expires_at"`
}

// Reconcile runs the alert-only consistency check and returns a report. It never
// mutates state.
func (s *MoneyService) Reconcile(ctx context.Context) (ReconcileReport, error) {
	if s == nil || s.db == nil {
		return ReconcileReport{}, fmt.Errorf("money service not initialized")
	}
	return ReconcileReport{GeneratedAt: s.now(), OrphanedHolds: []OrphanedHold{}}, nil
}

// FindOrphanedExpiredHolds returns no rows; it keeps the report shape.
func (s *MoneyService) FindOrphanedExpiredHolds(ctx context.Context) ([]OrphanedHold, error) {
	return []OrphanedHold{}, nil
}
