package credits

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/tenant"
	"github.com/uptrace/bun"
)

// Arrears transaction types (postpaid usage ledger, issue #241).
const (
	txOwedAccrual = "owed_accrual" // usage accrued to outstanding owed (positive amount)
	txOwedPayment = "owed_payment" // owed collected via a card charge (negative amount)
)

// AccrueOwed records postpaid usage against an arrears account: instead of
// withdrawing prepaid balance, the cost is added to outstanding_owed_cents and a
// ledger row is written. Idempotent on (owner, credit_type, source, source_id).
// The owner's outstanding ceiling is enforced separately at authorize time
// (CheckSpendAllowed); this is the settlement side. (#241)
func (s *CreditsService) AccrueOwed(ctx context.Context, owner identity.OwnerOrgID, creditType, source, sourceID string, amount int64) (*models.CreditTransaction, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("credits service not initialized")
	}
	if amount <= 0 {
		return nil, fmt.Errorf("amount must be positive")
	}
	source = strings.TrimSpace(source)
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return nil, fmt.Errorf("source_id required for owed accrual idempotency")
	}
	ct, err := s.GetCreditTypeByName(ctx, creditType)
	if err != nil {
		return nil, err
	}
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	ownerID := owner.UUID()
	now := s.now()

	tx, err := s.db.GetDB().(*bun.DB).BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// Idempotency.
	existing := new(models.CreditTransaction)
	err = tx.NewSelect().Model(existing).
		Where("tenant_id = ? AND owner_id = ? AND credit_type_id = ?", tenantID, ownerID, ct.ID).
		Where("transaction_type = ?", txOwedAccrual).
		Where("source = ? AND source_id = ?", source, sourceID).
		Limit(1).Scan(ctx)
	if err == nil {
		return existing, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	if err := s.ensureSettingsRowTx(ctx, tx, tenantID, ownerID, ct.ID, BillingModeArrears, now); err != nil {
		return nil, err
	}
	if _, err := tx.NewUpdate().Model((*models.CreditAccountSettings)(nil)).
		Set("outstanding_owed_cents = outstanding_owed_cents + ?", amount).
		Set("updated_at = ?", now).
		Where("tenant_id = ? AND owner_id = ? AND credit_type_id = ?", tenantID, ownerID, ct.ID).
		Exec(ctx); err != nil {
		return nil, err
	}

	trx := &models.CreditTransaction{
		ID: uuidutil.NewV7(), TenantID: tenantID, OwnerID: ownerID, UserID: ownerID.String(),
		CreditTypeID: ct.ID, Amount: amount, TransactionType: txOwedAccrual, Status: "posted",
		Source: source, SourceID: &sourceID, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := tx.NewInsert().Model(trx).Exec(ctx); err != nil {
		return nil, err
	}
	return trx, tx.Commit()
}

// ensureSettingsRowTx inserts a default settings row for (owner, credit_type) if
// one does not exist, using the given billing mode. No-op when the row exists.
func (s *CreditsService) ensureSettingsRowTx(ctx context.Context, tx bun.Tx, tenantID, ownerID, creditTypeID uuid.UUID, mode string, now time.Time) error {
	row := &models.CreditAccountSettings{
		ID: uuidutil.NewV7(), TenantID: tenantID, OwnerID: ownerID, CreditTypeID: creditTypeID,
		BillingMode: mode, HardStopOnBreach: true, AlertThresholdPct: 80,
		CreatedAt: now, UpdatedAt: now,
	}
	_, err := tx.NewInsert().Model(row).
		On("CONFLICT (tenant_id, owner_id, credit_type_id) DO NOTHING").Exec(ctx)
	return err
}

// GetOutstandingOwed returns the current outstanding owed for (owner, credit_type).
func (s *CreditsService) GetOutstandingOwed(ctx context.Context, owner identity.OwnerOrgID, creditType string) (int64, error) {
	settings, err := s.GetAccountSettings(ctx, owner, creditType)
	if err != nil {
		return 0, err
	}
	return settings.OutstandingOwedCents, nil
}

// arrearsAccount is a scanned arrears-account row for the charge job.
type arrearsAccount struct {
	TenantID        uuid.UUID  `bun:"tenant_id"`
	OwnerID         uuid.UUID  `bun:"owner_id"`
	CreditTypeID    uuid.UUID  `bun:"credit_type_id"`
	CreditTypeName  string     `bun:"credit_type_name"`
	Owed            int64      `bun:"outstanding_owed_cents"`
	PaymentMethodID *uuid.UUID `bun:"auto_topup_payment_method_id"`
}

// ChargeOutstanding collects outstanding owed for arrears accounts by charging
// the card on file. When minThresholdCents > 0 only accounts owing at least that
// much are charged (the threshold trigger, e.g. $500); minThresholdCents <= 0
// charges every account with owed > 0 (the month-end sweep). The charge is
// idempotent per (owner, credit_type, owed-snapshot). On success the owed is
// reduced by the charged amount and an owed_payment row is recorded; declines
// leave the owed in place for the next run. Returns the number of accounts
// successfully charged. (#241)
func (s *CreditsService) ChargeOutstanding(ctx context.Context, charger Charger, minThresholdCents int64) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("credits service not initialized")
	}
	if charger == nil {
		return 0, fmt.Errorf("charger required")
	}
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	q := s.db.GetDB().NewSelect().
		ColumnExpr("s.tenant_id, s.owner_id, s.credit_type_id, ct.name AS credit_type_name, s.outstanding_owed_cents, s.auto_topup_payment_method_id").
		TableExpr("billing.credit_account_settings AS s").
		Join("JOIN billing.credit_types AS ct ON ct.id = s.credit_type_id").
		Where("s.tenant_id = ?", tenantID).
		Where("s.billing_mode = ?", BillingModeArrears).
		Where("s.outstanding_owed_cents > 0").
		Where("s.auto_topup_payment_method_id IS NOT NULL")
	if minThresholdCents > 0 {
		q = q.Where("s.outstanding_owed_cents >= ?", minThresholdCents)
	}
	var rows []arrearsAccount
	if err := q.Scan(ctx, &rows); err != nil {
		return 0, err
	}

	count := 0
	for _, r := range rows {
		ok, err := s.chargeOneOutstanding(ctx, charger, r)
		if err != nil {
			return count, err
		}
		if ok {
			count++
		}
	}
	return count, nil
}

func (s *CreditsService) chargeOneOutstanding(ctx context.Context, charger Charger, r arrearsAccount) (bool, error) {
	owner := identity.OwnerOrgID(r.OwnerID)
	snapshot := r.Owed
	if snapshot <= 0 || r.PaymentMethodID == nil {
		return false, nil
	}
	key := fmt.Sprintf("arrears:%s:%s:%d", r.OwnerID, r.CreditTypeID, snapshot)

	res, err := charger.ChargeSavedMethod(ctx, ChargeRequest{
		Owner:           owner,
		UserID:          owner.UUID().String(),
		PaymentMethodID: *r.PaymentMethodID,
		AmountCents:     snapshot,
		IdempotencyKey:  key,
		Description:     "outstanding balance: " + r.CreditTypeName,
	})
	if err != nil {
		return false, err
	}
	if res.Declined {
		return false, nil // leave owed; dunning/next run retries
	}

	now := s.now()
	tx, err := s.db.GetDB().(*bun.DB).BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	// Reduce owed by the snapshot (never below zero); CAS-style guard prevents a
	// double-decrement if two runs race on the same snapshot.
	upd, err := tx.NewUpdate().Model((*models.CreditAccountSettings)(nil)).
		Set("outstanding_owed_cents = GREATEST(0, outstanding_owed_cents - ?)", snapshot).
		Set("updated_at = ?", now).
		Where("tenant_id = ? AND owner_id = ? AND credit_type_id = ?", r.TenantID, r.OwnerID, r.CreditTypeID).
		Where("outstanding_owed_cents >= ?", snapshot).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	if n, _ := upd.RowsAffected(); n == 0 {
		// Owed already reduced by a concurrent run for this snapshot; treat as done.
		return false, tx.Commit()
	}

	sid := key
	trx := &models.CreditTransaction{
		ID: uuidutil.NewV7(), TenantID: r.TenantID, OwnerID: r.OwnerID, UserID: r.OwnerID.String(),
		CreditTypeID: r.CreditTypeID, Amount: -snapshot, TransactionType: txOwedPayment, Status: "posted",
		Source: "arrears_charge", SourceID: &sid, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := tx.NewInsert().Model(trx).
		On("CONFLICT DO NOTHING").Exec(ctx); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
