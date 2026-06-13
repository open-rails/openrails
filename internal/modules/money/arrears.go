package money

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/tenant"
)

// Arrears transaction types (postpaid usage ledger, issue #241).
const (
	txOwedAccrual = "owed_accrual" // usage accrued to outstanding owed (positive amount)
	txOwedPayment = "owed_payment" // owed collected via a card charge (negative amount)
)

// AccrueOwed records postpaid usage against an arrears account: instead of
// withdrawing prepaid balance, the cost is added to outstanding_owed_micros and a
// ledger row is written. Idempotent on (payer, source, source_id).
// The payer's outstanding ceiling is enforced separately at authorize time
// (CheckSpendAllowed); this is the settlement side. (#241)
func (s *MoneyService) AccrueOwed(ctx context.Context, payer identity.TenantSubjectID, currency, source, sourceID string, amount int64) (*models.MoneyTransaction, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if amount <= 0 {
		return nil, fmt.Errorf("amount must be positive")
	}
	// #474 invariant: owed/arrears is external-currency-only (reject custom credits).
	cur := normalizeCurrency(currency)
	if err := RequireBillingCurrency(cur); err != nil {
		return nil, err
	}
	source = strings.TrimSpace(source)
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return nil, fmt.Errorf("source_id required for owed accrual idempotency")
	}
	tid, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()
	now := s.now()

	var trx *models.MoneyTransaction
	// Privileged (no-GUC) transaction: this path runs with explicit tenant_id
	// predicates, matching the bun-era plain BeginTx.
	err = s.db.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)

		// Idempotency.
		existing, gerr := q.GetMoneyTransactionByCoords(ctx, gen.GetMoneyTransactionByCoordsParams{
			TenantID: tenantID, TenantSubjectID: payerID, Currency: cur,
			TransactionType: txOwedAccrual, Source: source, SourceID: &sourceID,
		})
		if gerr == nil {
			trx, gerr = moneyTransactionFromGen(existing)
			return gerr
		}
		if !errors.Is(gerr, pgx.ErrNoRows) {
			return gerr
		}

		if err := s.ensureSettingsRowTx(ctx, q, tenantID, payerID, cur, BillingModeArrears, now); err != nil {
			return err
		}
		if err := q.AddMoneyOutstandingOwed(ctx, gen.AddMoneyOutstandingOwedParams{
			TenantID: tenantID, TenantSubjectID: payerID, Currency: cur,
			Amount: amount, Now: now,
		}); err != nil {
			return err
		}

		trx = &models.MoneyTransaction{
			ID: uuidutil.NewV7(), TenantID: tenantID, TenantSubjectID: payerID, Currency: cur, Actor: payerID.String(),
			Amount: amount, TransactionType: txOwedAccrual, Status: "posted",
			Source: source, SourceID: &sourceID, CreatedAt: now, UpdatedAt: now,
		}
		return q.InsertMoneyTransaction(ctx, insertParamsFromTransaction(trx))
	})
	if err != nil {
		return nil, err
	}
	return trx, nil
}

// ensureSettingsRowTx inserts a default settings row for payer if one does not
// exist, using the given billing mode. No-op when the row exists.
func (s *MoneyService) ensureSettingsRowTx(ctx context.Context, q *gen.Queries, tenantID, payerID uuid.UUID, currency, mode string, now time.Time) error {
	// Materialize the payable tenant_subjects row so the money_accounts FK
	// (migration 076) is satisfied — this is the shared choke point for settings
	// writes (suspend/resume/verify/graduate/arrears) (#317).
	if err := ensureTenantSubject(ctx, q, tenantID, payerID); err != nil {
		return err
	}
	return q.InsertMoneyAccountSettingsIfAbsent(ctx, gen.InsertMoneyAccountSettingsIfAbsentParams{
		ID: uuidutil.NewV7(), TenantID: tenantID, TenantSubjectID: payerID, Currency: normalizeCurrency(currency),
		BillingMode: mode, Now: now,
	})
}

// GetOutstandingOwed returns the current outstanding owed for payer in currency.
func (s *MoneyService) GetOutstandingOwed(ctx context.Context, payer identity.TenantSubjectID, currency string) (int64, error) {
	settings, err := s.getAccountSettings(ctx, payer, currency)
	if err != nil {
		return 0, err
	}
	return settings.OutstandingOwedMicros, nil
}

// arrearsAccount is a scanned arrears-account row for the charge job.
type arrearsAccount struct {
	TenantID        uuid.UUID
	TenantSubjectID uuid.UUID
	Currency        string
	Owed            int64
	PaymentMethodID *uuid.UUID
}

// ChargeOutstanding collects outstanding owed for arrears accounts by charging
// the card on file. When minThresholdMicros > 0 only accounts owing at least that
// much are charged (the threshold trigger, e.g. $500); minThresholdMicros <= 0
// charges every account with owed > 0 (the month-end sweep). The charge is
// idempotent per (payer, owed-snapshot). On success the owed is reduced by the
// charged amount and an owed_payment row is recorded; declines leave the owed in
// place for the next run. Returns the number of accounts successfully charged. (#241)
func (s *MoneyService) ChargeOutstanding(ctx context.Context, charger Charger, minThresholdMicros int64) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("money service not initialized")
	}
	if charger == nil {
		return 0, fmt.Errorf("charger required")
	}
	tid, err := tenant.Require(ctx)
	if err != nil {
		return 0, err
	}
	tenantID := tid.UUID()
	genRows, err := s.db.Gen(ctx).ListChargeableArrearsMoneyAccounts(ctx, gen.ListChargeableArrearsMoneyAccountsParams{
		TenantID: tenantID, MinThreshold: minThresholdMicros,
	})
	if err != nil {
		return 0, err
	}
	rows := make([]arrearsAccount, 0, len(genRows))
	for _, r := range genRows {
		rows = append(rows, arrearsAccount{
			TenantID:        r.TenantID,
			TenantSubjectID: r.TenantSubjectID,
			Currency:        r.Currency,
			Owed:            r.OutstandingOwedMicros,
			PaymentMethodID: r.AutoTopupPaymentMethodID,
		})
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

func (s *MoneyService) chargeOneOutstanding(ctx context.Context, charger Charger, r arrearsAccount) (bool, error) {
	payer := identity.TenantSubjectID(r.TenantSubjectID)
	snapshot := r.Owed
	if snapshot <= 0 || r.PaymentMethodID == nil {
		return false, nil
	}
	// #474 invariant: never charge a card for a custom-credit (non-currency) balance.
	if err := RequireBillingCurrency(normalizeCurrency(r.Currency)); err != nil {
		return false, nil
	}
	key := fmt.Sprintf("arrears:%s:%d", r.TenantSubjectID, snapshot)

	// Owed is in ledger micro-dollars; the processor charges whole cents
	// (1 cent = 10,000 micros). Round up so we never under-collect.
	res, err := charger.ChargeSavedMethod(ctx, ChargeRequest{
		Payer:           payer,
		Actor:           payer.UUID().String(),
		PaymentMethodID: *r.PaymentMethodID,
		AmountCents:     (snapshot + 9_999) / 10_000,
		IdempotencyKey:  key,
		Description:     "outstanding balance",
	})
	if err != nil {
		return false, err
	}
	if res.Declined {
		return false, nil // leave owed; dunning/next run retries
	}

	now := s.now()
	charged := false
	// Privileged (no-GUC) transaction, matching the bun-era plain BeginTx.
	err = s.db.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)

		// Reduce owed by the snapshot (never below zero); CAS-style guard prevents a
		// double-decrement if two runs race on the same snapshot.
		n, uerr := q.ReduceMoneyOutstandingOwedSnapshot(ctx, gen.ReduceMoneyOutstandingOwedSnapshotParams{
			TenantID: r.TenantID, TenantSubjectID: r.TenantSubjectID, Currency: normalizeCurrency(r.Currency),
			Snapshot: snapshot, Now: now,
		})
		if uerr != nil {
			return uerr
		}
		if n == 0 {
			// Owed already reduced by a concurrent run for this snapshot; treat as done.
			return nil
		}

		sid := key
		trx := &models.MoneyTransaction{
			ID: uuidutil.NewV7(), TenantID: r.TenantID, TenantSubjectID: r.TenantSubjectID, Currency: normalizeCurrency(r.Currency), Actor: r.TenantSubjectID.String(),
			Amount: -snapshot, TransactionType: txOwedPayment, Status: "posted",
			Source: "arrears_charge", SourceID: &sid, CreatedAt: now, UpdatedAt: now,
		}
		if err := q.InsertMoneyTransactionIfAbsent(ctx, gen.InsertMoneyTransactionIfAbsentParams(insertParamsFromTransaction(trx))); err != nil {
			return err
		}
		charged = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return charged, nil
}
