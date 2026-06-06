package credits

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/tenant"
)

// HeldBalanceDrift is a balance row whose stored held_balance disagrees with the
// sum of its currently-active holds.
type HeldBalanceDrift struct {
	TenantID        uuid.UUID `bun:"tenant_id" json:"tenant_id"`
	TenantSubjectID uuid.UUID `bun:"tenant_subject_id" json:"tenant_subject_id"`
	CreditTypeID    uuid.UUID `bun:"credit_type_id" json:"credit_type_id"`
	Stored          int64     `bun:"stored" json:"stored_held_balance"`
	Computed        int64     `bun:"computed" json:"computed_held_balance"`
}

// BalanceAnomaly flags a balance row that violates a hard invariant.
type BalanceAnomaly struct {
	TenantID        uuid.UUID `bun:"tenant_id" json:"tenant_id"`
	TenantSubjectID uuid.UUID `bun:"tenant_subject_id" json:"tenant_subject_id"`
	CreditTypeID    uuid.UUID `bun:"credit_type_id" json:"credit_type_id"`
	Balance         int64     `bun:"balance" json:"balance"`
	HeldBalance     int64     `bun:"held_balance" json:"held_balance"`
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
	TenantSubjectID uuid.UUID  `json:"tenant_subject_id"`
	Amount          int64      `json:"authorized_amount"`
	ExpiresAt       *time.Time `json:"expires_at"`
}

// Reconcile runs all alert-only consistency checks and returns a report. It
// never mutates state (issue #243, alert-first). Use RepairHeldBalance for the
// safe auto-repair once an operator has reviewed the drift.
func (s *CreditsService) Reconcile(ctx context.Context) (ReconcileReport, error) {
	if s == nil || s.db == nil {
		return ReconcileReport{}, fmt.Errorf("credits service not initialized")
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
func (s *CreditsService) FindOrphanedExpiredHolds(ctx context.Context) ([]OrphanedHold, error) {
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	now := s.now()
	var holds []models.CreditTransaction
	err := s.db.Q(ctx).NewSelect().Model(&holds).
		Where("tenant_id = ?", tenantID).
		Where("transaction_type = 'hold' AND status = 'active'").
		Where("expires_at IS NOT NULL AND expires_at <= ?", now).
		OrderExpr("expires_at ASC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]OrphanedHold, 0, len(holds))
	for i := range holds {
		var amt int64
		if holds[i].Authorized != nil {
			amt = *holds[i].Authorized
		}
		out = append(out, OrphanedHold{ID: holds[i].ID, TenantSubjectID: holds[i].TenantSubjectID, Amount: amt, ExpiresAt: holds[i].ExpiresAt})
	}
	return out, nil
}

const activeHoldSumSubquery = `COALESCE((SELECT SUM(COALESCE(t.authorized_amount,0)) ` +
	`FROM billing.credit_transactions t ` +
	`WHERE t.tenant_id = b.tenant_id AND t.tenant_subject_id = b.tenant_subject_id AND t.credit_type_id = b.credit_type_id ` +
	`AND t.transaction_type = 'hold' AND t.status = 'active'), 0)`

// FindHeldBalanceDrift returns balance rows whose stored held_balance differs
// from the sum of their active holds.
func (s *CreditsService) FindHeldBalanceDrift(ctx context.Context) ([]HeldBalanceDrift, error) {
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	var rows []HeldBalanceDrift
	err := s.db.Q(ctx).NewSelect().
		ColumnExpr("b.tenant_id, b.tenant_subject_id, b.credit_type_id, b.held_balance AS stored").
		ColumnExpr(activeHoldSumSubquery+" AS computed").
		TableExpr("billing.user_credit_balances AS b").
		Where("b.tenant_id = ?", tenantID).
		Where("b.held_balance <> "+activeHoldSumSubquery).
		Scan(ctx, &rows)
	return rows, err
}

// FindBalanceAnomalies returns balance rows violating hard invariants:
// balance < 0, held_balance < 0, or held_balance > balance.
func (s *CreditsService) FindBalanceAnomalies(ctx context.Context) ([]BalanceAnomaly, error) {
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	var rows []BalanceAnomaly
	err := s.db.Q(ctx).NewSelect().
		ColumnExpr("b.tenant_id, b.tenant_subject_id, b.credit_type_id, b.balance, b.held_balance").
		TableExpr("billing.user_credit_balances AS b").
		Where("b.tenant_id = ?", tenantID).
		Where("b.balance < 0 OR b.held_balance < 0 OR b.held_balance > b.balance").
		Scan(ctx, &rows)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		switch {
		case rows[i].Balance < 0:
			rows[i].Reason = "negative_balance"
		case rows[i].HeldBalance < 0:
			rows[i].Reason = "negative_held_balance"
		default:
			rows[i].Reason = "held_exceeds_balance"
		}
	}
	return rows, nil
}

// RepairHeldBalance recomputes held_balance for (payer, credit_type) from the sum
// of active holds and writes it. This is the safe auto-repair for HeldBalanceDrift
// (the active holds are the source of truth). Returns the corrected value.
func (s *CreditsService) RepairHeldBalance(ctx context.Context, payer identity.TenantSubjectID, creditType string) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("credits service not initialized")
	}
	ct, err := s.GetCreditTypeByName(ctx, creditType)
	if err != nil {
		return 0, err
	}
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	ownerID := payer.UUID()
	computed, err := s.activeHoldsTotal(ctx, tenantID, ownerID, ct.ID)
	if err != nil {
		return 0, err
	}
	_, err = s.db.Q(ctx).NewUpdate().Model((*models.UserCreditBalance)(nil)).
		Set("held_balance = ?", computed).
		Set("updated_at = ?", s.now()).
		Where("tenant_id = ? AND tenant_subject_id = ? AND credit_type_id = ?", tenantID, ownerID, ct.ID).
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	return computed, nil
}
