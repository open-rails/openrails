package money

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// HeldBalanceDrift is a balance row whose stored held_balance disagrees with the
// sum of its currently-active holds.
type HeldBalanceDrift struct {
	MerchantID        uuid.UUID `json:"tenant_id"`
	MerchantSubjectID uuid.UUID `json:"tenant_subject_id"`
	Currency        string    `json:"currency"`
	Stored          int64     `json:"stored_held_balance"`
	Computed        int64     `json:"computed_held_balance"`
}

// BalanceAnomaly flags a balance row that violates a hard invariant.
type BalanceAnomaly struct {
	MerchantID        uuid.UUID `json:"tenant_id"`
	MerchantSubjectID uuid.UUID `json:"tenant_subject_id"`
	Currency        string    `json:"currency"`
	Balance         int64     `json:"balance"`
	HeldBalance     int64     `json:"held_balance"`
	Reason          string    `json:"reason"`
}

// ReconcileReport is the alert-only output of Reconcile. Empty slices mean the
// ledger is internally consistent.
type ReconcileReport struct {
	GeneratedAt      time.Time          `json:"generated_at"`
	OrphanedHolds    []OrphanedHold     `json:"orphaned_holds"`
	HeldBalanceDrift []HeldBalanceDrift `json:"held_balance_drift"`
	BalanceAnomalies []BalanceAnomaly   `json:"balance_anomalies"`
}

// OrphanedHold is an active hold that has passed its expiry and should have been
// released by HoldExpiryWorker.
type OrphanedHold struct {
	ID              uuid.UUID  `json:"id"`
	MerchantSubjectID uuid.UUID  `json:"tenant_subject_id"`
	Amount          int64      `json:"authorized_amount"`
	ExpiresAt       *time.Time `json:"expires_at"`
}

// Reconcile runs all alert-only consistency checks and returns a report. It
// never mutates state (issue #243, alert-first). Use RepairHeldBalance for the
// safe auto-repair once an operator has reviewed the drift.
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

	drift, err := s.FindHeldBalanceDrift(ctx)
	if err != nil {
		return rep, err
	}
	rep.HeldBalanceDrift = drift

	anomalies, err := s.FindBalanceAnomalies(ctx)
	if err != nil {
		return rep, err
	}
	rep.BalanceAnomalies = anomalies

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
		out = append(out, OrphanedHold{ID: holds[i].ID, MerchantSubjectID: holds[i].MerchantSubjectID, Amount: amt, ExpiresAt: holds[i].ExpiresAt})
	}
	return out, nil
}

// FindHeldBalanceDrift returns balance rows whose stored held_balance differs
// from the sum of their active holds.
func (s *MoneyService) FindHeldBalanceDrift(ctx context.Context) ([]HeldBalanceDrift, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tenantID := tid.UUID()
	rows, err := s.db.Gen(ctx).ListMoneyHeldBalanceDrift(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]HeldBalanceDrift, 0, len(rows))
	for _, r := range rows {
		out = append(out, HeldBalanceDrift{
			MerchantID:        r.MerchantID,
			MerchantSubjectID: r.MerchantSubjectID,
			Currency:        r.Currency,
			Stored:          r.Stored,
			Computed:        r.Computed,
		})
	}
	return out, nil
}

// FindBalanceAnomalies returns balance rows violating hard invariants:
// balance < 0, held_balance < 0, or held_balance > balance.
func (s *MoneyService) FindBalanceAnomalies(ctx context.Context) ([]BalanceAnomaly, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tenantID := tid.UUID()
	rows, err := s.db.Gen(ctx).ListMoneyBalanceAnomalies(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]BalanceAnomaly, 0, len(rows))
	for _, r := range rows {
		a := BalanceAnomaly{
			MerchantID:        r.MerchantID,
			MerchantSubjectID: r.MerchantSubjectID,
			Currency:        r.Currency,
			Balance:         r.Balance,
			HeldBalance:     r.HeldBalance,
		}
		switch {
		case a.Balance < 0:
			a.Reason = "negative_balance"
		case a.HeldBalance < 0:
			a.Reason = "negative_held_balance"
		default:
			a.Reason = "held_exceeds_balance"
		}
		out = append(out, a)
	}
	return out, nil
}

// RepairHeldBalance recomputes held_balance for (payer, credit_type) from the sum
// of active holds and writes it. This is the safe auto-repair for HeldBalanceDrift
// (the active holds are the source of truth). Returns the corrected value.
func (s *MoneyService) RepairHeldBalance(ctx context.Context, payer identity.MerchantSubjectID, currency string) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("money service not initialized")
	}
	cur := normalizeCurrency(currency)
	if err := ValidateCurrency(cur); err != nil {
		return 0, err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()
	computed, err := s.activeHoldsTotal(ctx, tenantID, payerID, cur)
	if err != nil {
		return 0, err
	}
	if err := s.db.Gen(ctx).SetMoneyHeldBalance(ctx, gen.SetMoneyHeldBalanceParams{
		MerchantID: tenantID, MerchantSubjectID: payerID, Currency: cur,
		HeldBalance: computed, UpdatedAt: s.now(),
	}); err != nil {
		return 0, err
	}
	return computed, nil
}
