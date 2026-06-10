package credits

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
// ledger row is written. Idempotent on (payer, credit_type, source, source_id).
// The payer's outstanding ceiling is enforced separately at authorize time
// (CheckSpendAllowed); this is the settlement side. (#241)
func (s *CreditsService) AccrueOwed(ctx context.Context, payer identity.TenantSubjectID, creditType, source, sourceID string, amount int64) (*models.CreditTransaction, error) {
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
	payerID := payer.UUID()
	now := s.now()

	var trx *models.CreditTransaction
	// Privileged (no-GUC) transaction: this path runs with explicit tenant_id
	// predicates, matching the bun-era plain BeginTx.
	err = s.db.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)

		// Idempotency.
		existing, gerr := q.GetCreditTransactionByCoords(ctx, gen.GetCreditTransactionByCoordsParams{
			TenantID: tenantID, TenantSubjectID: payerID, CreditTypeID: ct.ID,
			TransactionType: txOwedAccrual, Source: source, SourceID: &sourceID,
		})
		if gerr == nil {
			trx, gerr = creditTransactionFromGen(existing)
			return gerr
		}
		if !errors.Is(gerr, pgx.ErrNoRows) {
			return gerr
		}

		if err := s.ensureSettingsRowTx(ctx, q, tenantID, payerID, ct.ID, BillingModeArrears, now); err != nil {
			return err
		}
		if err := q.AddOutstandingOwed(ctx, gen.AddOutstandingOwedParams{
			TenantID: tenantID, TenantSubjectID: payerID, CreditTypeID: ct.ID,
			Amount: amount, Now: now,
		}); err != nil {
			return err
		}

		trx = &models.CreditTransaction{
			ID: uuidutil.NewV7(), TenantID: tenantID, TenantSubjectID: payerID, Actor: payerID.String(),
			CreditTypeID: ct.ID, Amount: amount, TransactionType: txOwedAccrual, Status: "posted",
			Source: source, SourceID: &sourceID, CreatedAt: now, UpdatedAt: now,
		}
		return q.InsertCreditTransaction(ctx, insertParamsFromTransaction(trx))
	})
	if err != nil {
		return nil, err
	}
	return trx, nil
}

// ensureSettingsRowTx inserts a default settings row for (payer, credit_type) if
// one does not exist, using the given billing mode. No-op when the row exists.
func (s *CreditsService) ensureSettingsRowTx(ctx context.Context, q *gen.Queries, tenantID, payerID, creditTypeID uuid.UUID, mode string, now time.Time) error {
	// Materialize the payable tenant_subjects row so the credit_account_settings
	// FK (migration 076) is satisfied — this is the shared choke point for
	// settings writes (suspend/resume/verify/graduate/arrears) (#317).
	if err := ensureTenantSubject(ctx, q, tenantID, payerID); err != nil {
		return err
	}
	return q.InsertCreditAccountSettingsIfAbsent(ctx, gen.InsertCreditAccountSettingsIfAbsentParams{
		ID: uuidutil.NewV7(), TenantID: tenantID, TenantSubjectID: payerID,
		CreditTypeID: creditTypeID, BillingMode: mode, Now: now,
	})
}

// GetOutstandingOwed returns the current outstanding owed for (payer, credit_type).
func (s *CreditsService) GetOutstandingOwed(ctx context.Context, payer identity.TenantSubjectID, creditType string) (int64, error) {
	settings, err := s.GetAccountSettings(ctx, payer, creditType)
	if err != nil {
		return 0, err
	}
	return settings.OutstandingOwedMicros, nil
}

// arrearsAccount is a scanned arrears-account row for the charge job.
type arrearsAccount struct {
	TenantID        uuid.UUID
	TenantSubjectID uuid.UUID
	CreditTypeID    uuid.UUID
	CreditTypeName  string
	Owed            int64
	PaymentMethodID *uuid.UUID
}

// ChargeOutstanding collects outstanding owed for arrears accounts by charging
// the card on file. When minThresholdMicros > 0 only accounts owing at least that
// much are charged (the threshold trigger, e.g. $500); minThresholdMicros <= 0
// charges every account with owed > 0 (the month-end sweep). The charge is
// idempotent per (payer, credit_type, owed-snapshot). On success the owed is
// reduced by the charged amount and an owed_payment row is recorded; declines
// leave the owed in place for the next run. Returns the number of accounts
// successfully charged. (#241)
func (s *CreditsService) ChargeOutstanding(ctx context.Context, charger Charger, minThresholdMicros int64) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("credits service not initialized")
	}
	if charger == nil {
		return 0, fmt.Errorf("charger required")
	}
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	genRows, err := s.db.Gen(ctx).ListChargeableArrearsAccounts(ctx, gen.ListChargeableArrearsAccountsParams{
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
			CreditTypeID:    r.CreditTypeID,
			CreditTypeName:  r.CreditTypeName,
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

func (s *CreditsService) chargeOneOutstanding(ctx context.Context, charger Charger, r arrearsAccount) (bool, error) {
	payer := identity.TenantSubjectID(r.TenantSubjectID)
	snapshot := r.Owed
	if snapshot <= 0 || r.PaymentMethodID == nil {
		return false, nil
	}
	key := fmt.Sprintf("arrears:%s:%s:%d", r.TenantSubjectID, r.CreditTypeID, snapshot)

	// Owed is in ledger micro-dollars; the processor charges whole cents
	// (1 cent = 10,000 micros). Round up so we never under-collect.
	res, err := charger.ChargeSavedMethod(ctx, ChargeRequest{
		Payer:           payer,
		Actor:           payer.UUID().String(),
		PaymentMethodID: *r.PaymentMethodID,
		AmountCents:     (snapshot + 9_999) / 10_000,
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
	charged := false
	// Privileged (no-GUC) transaction, matching the bun-era plain BeginTx.
	err = s.db.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)

		// Reduce owed by the snapshot (never below zero); CAS-style guard prevents a
		// double-decrement if two runs race on the same snapshot.
		n, uerr := q.ReduceOutstandingOwedSnapshot(ctx, gen.ReduceOutstandingOwedSnapshotParams{
			TenantID: r.TenantID, TenantSubjectID: r.TenantSubjectID, CreditTypeID: r.CreditTypeID,
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
		trx := &models.CreditTransaction{
			ID: uuidutil.NewV7(), TenantID: r.TenantID, TenantSubjectID: r.TenantSubjectID, Actor: r.TenantSubjectID.String(),
			CreditTypeID: r.CreditTypeID, Amount: -snapshot, TransactionType: txOwedPayment, Status: "posted",
			Source: "arrears_charge", SourceID: &sid, CreatedAt: now, UpdatedAt: now,
		}
		if err := q.InsertCreditTransactionIfAbsent(ctx, gen.InsertCreditTransactionIfAbsentParams(insertParamsFromTransaction(trx))); err != nil {
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
