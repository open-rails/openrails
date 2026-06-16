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
	"github.com/open-rails/openrails/pkg/merchant"
)

// Arrears transaction types (postpaid usage ledger, issue #241).
const (
	txOwedAccrual = "owed_accrual" // usage accrued to outstanding owed (positive amount)
	txOwedPayment = "owed_payment" // owed collected via a card charge (negative amount)
)

// AccrueOwed records postpaid usage against an arrears account: instead of
// withdrawing prepaid balance, the cost is added to outstanding_owed_amount and a
// ledger row is written. Idempotent on (payer, source, source_id).
// The payer's outstanding ceiling is enforced separately at authorize time
// (CheckSpendAllowed); this is the settlement side. (#241)
func (s *MoneyService) AccrueOwed(ctx context.Context, payer identity.CustomerID, currency, source, sourceID string, amount int64) (*models.MoneyTransaction, error) {
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
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()
	now := s.now()

	var trx *models.MoneyTransaction
	// Privileged (no-GUC) transaction: this path runs with explicit merchant_id
	// predicates, matching the bun-era plain BeginTx.
	err = s.db.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)

		// Idempotency.
		existing, gerr := q.GetMoneyTransactionByCoords(ctx, gen.GetMoneyTransactionByCoordsParams{
			MerchantID: tenantID, CustomerID: payerID, Currency: cur,
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
			MerchantID: tenantID, CustomerID: payerID, Currency: cur,
			Amount: amount, Now: now,
		}); err != nil {
			return err
		}

		trx = &models.MoneyTransaction{
			ID: uuidutil.NewV7(), MerchantID: tenantID, CustomerID: payerID, Currency: cur, Invoker: payerID.String(),
			Amount: amount, TransactionType: txOwedAccrual, Status: "posted",
			Source: source, SourceID: &sourceID, CreatedAt: now, UpdatedAt: now,
		}
		if err := q.InsertMoneyTransaction(ctx, insertParamsFromTransaction(trx)); err != nil {
			return err
		}
		return insertPendingInvoiceItemTx(ctx, q, tenantID, payerID, cur, txOwedAccrual, source+":"+sourceID, nil, amount, now, map[string]any{
			"source": source,
		})
	})
	if err != nil {
		return nil, err
	}
	return trx, nil
}

// ensureSettingsRowTx inserts a default settings row for payer if one does not
// exist, using the given billing mode. No-op when the row exists.
func (s *MoneyService) ensureSettingsRowTx(ctx context.Context, q *gen.Queries, tenantID, payerID uuid.UUID, currency, mode string, now time.Time) error {
	// Materialize the payable customers row so the money_accounts FK
	// (migration 076) is satisfied — this is the shared choke point for settings
	// writes (suspend/resume/verify/graduate/arrears) (#317).
	if err := ensureCustomer(ctx, q, tenantID, payerID); err != nil {
		return err
	}
	return q.InsertMoneyAccountSettingsIfAbsent(ctx, gen.InsertMoneyAccountSettingsIfAbsentParams{
		ID: uuidutil.NewV7(), MerchantID: tenantID, CustomerID: payerID, Currency: normalizeCurrency(currency),
		BillingMode: mode, Now: now,
	})
}

// SetCreditLimit sets the admin/operator arrears credit line for a payer (#489):
// under billing_mode=arrears the balance may go negative up to creditLimit;
// AdmitHold denies insufficient_credit when a new hold would exceed remaining
// capacity. 0 means no arrears capacity, although prepaid balance may still be
// spent. This is OPERATOR-only — deliberately NOT part of the self-serve
// UpsertAccountSettings surface. It ensures a settings row exists, then stamps
// the limit.
func (s *MoneyService) SetCreditLimit(ctx context.Context, payer identity.CustomerID, currency string, creditLimit int64) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return fmt.Errorf("payer required")
	}
	if creditLimit < 0 {
		return fmt.Errorf("credit_limit_amount must be >= 0")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	tenantID := tid.UUID()
	now := s.now()
	cur := normalizeCurrency(currency)
	if err := RequireBillingCurrency(cur); err != nil {
		return err
	}
	return s.db.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		// Ensure a settings row exists (arrears mode if creating — a credit line
		// only matters for arrears; no-op when the row already exists).
		if err := s.ensureSettingsRowTx(ctx, q, tenantID, payer.UUID(), cur, BillingModeArrears, now); err != nil {
			return err
		}
		return q.SetMoneyAccountCreditLimit(ctx, gen.SetMoneyAccountCreditLimitParams{
			MerchantID: tenantID, CustomerID: payer.UUID(), Currency: cur,
			CreditLimit: creditLimit, Now: now,
		})
	})
}

// GetCreditLimit returns the admin-set arrears credit line for a payer (#489).
func (s *MoneyService) GetCreditLimit(ctx context.Context, payer identity.CustomerID, currency string) (int64, error) {
	settings, err := s.GetAccountSettings(ctx, payer, currency)
	if err != nil {
		return 0, err
	}
	return settings.CreditLimitAmount, nil
}

// GetOutstandingOwed returns the current outstanding owed for payer in currency.
func (s *MoneyService) GetOutstandingOwed(ctx context.Context, payer identity.CustomerID, currency string) (int64, error) {
	settings, err := s.getAccountSettings(ctx, payer, currency)
	if err != nil {
		return 0, err
	}
	return settings.OutstandingOwedAmount, nil
}

// arrearsAccount is a scanned arrears-account row for the charge job.
type arrearsAccount struct {
	MerchantID      uuid.UUID
	CustomerID      uuid.UUID
	Currency        string
	Owed            int64
	PaymentMethodID *uuid.UUID
}

type invoiceArrearsAccount struct {
	InvoiceID       uuid.UUID
	MerchantID      uuid.UUID
	CustomerID      uuid.UUID
	Currency        string
	AmountDue       int64
	PaymentMethodID *uuid.UUID
}

// ChargeOutstanding collects outstanding owed for arrears accounts by charging
// the card on file. When minThreshold > 0 only accounts owing at least that
// much are charged (the threshold trigger, e.g. $500); minThreshold <= 0
// charges every account with owed > 0 (the month-end sweep). Invoiced debt is
// collected first and payments are allocated to invoice_id; legacy uninvoiced
// owed balances remain as a fallback until #506 fully removes the live owed
// counter. Returns the number of successful invoice/account charges. (#241/#506)
func (s *MoneyService) ChargeOutstanding(ctx context.Context, charger Charger, minThreshold int64) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("money service not initialized")
	}
	if charger == nil {
		return 0, fmt.Errorf("charger required")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	tenantID := tid.UUID()
	invoiceRows, err := s.db.Gen(ctx).ListChargeableOpenInvoices(ctx, gen.ListChargeableOpenInvoicesParams{
		MerchantID: tenantID, MinThreshold: minThreshold,
	})
	if err != nil {
		return 0, err
	}
	count := 0
	for _, r := range invoiceRows {
		ok, err := s.chargeOneOpenInvoice(ctx, charger, invoiceArrearsAccount{
			InvoiceID:       r.ID,
			MerchantID:      r.MerchantID,
			CustomerID:      r.CustomerID,
			Currency:        r.Currency,
			AmountDue:       r.AmountDue,
			PaymentMethodID: r.AutoTopupPaymentMethodID,
		})
		if err != nil {
			return count, err
		}
		if ok {
			count++
		}
	}

	genRows, err := s.db.Gen(ctx).ListChargeableArrearsMoneyAccounts(ctx, gen.ListChargeableArrearsMoneyAccountsParams{
		MerchantID: tenantID, MinThreshold: minThreshold,
	})
	if err != nil {
		return 0, err
	}
	rows := make([]arrearsAccount, 0, len(genRows))
	for _, r := range genRows {
		rows = append(rows, arrearsAccount{
			MerchantID:      r.MerchantID,
			CustomerID:      r.CustomerID,
			Currency:        r.Currency,
			Owed:            r.OutstandingOwedAmount,
			PaymentMethodID: r.AutoTopupPaymentMethodID,
		})
	}

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

func (s *MoneyService) chargeOneOpenInvoice(ctx context.Context, charger Charger, r invoiceArrearsAccount) (bool, error) {
	payer := identity.CustomerID(r.CustomerID)
	snapshot := r.AmountDue
	if snapshot <= 0 || r.PaymentMethodID == nil {
		return false, nil
	}
	if err := RequireBillingCurrency(normalizeCurrency(r.Currency)); err != nil {
		return false, nil
	}
	key := fmt.Sprintf("invoice:%s:%d", r.InvoiceID, snapshot)
	res, err := charger.ChargeSavedMethod(ctx, ChargeRequest{
		MerchantID:      r.MerchantID,
		Payer:           payer,
		Invoker:         payer.UUID().String(),
		InvoiceID:       &r.InvoiceID,
		PaymentMethodID: *r.PaymentMethodID,
		AmountCents:     (snapshot + 9_999) / 10_000,
		Currency:        normalizeCurrency(r.Currency),
		IdempotencyKey:  key,
		Description:     fmt.Sprintf("invoice %s", r.InvoiceID.String()),
	})
	if err != nil {
		return false, err
	}
	now := s.now()
	if res.Declined {
		if err := s.recordInvoicePaymentAttempt(ctx, r, nil, "failed", optionalProcessor(res.Processor), optionalString(res.TransactionID), res.FailureCode, res.FailureMessage, now); err != nil {
			return false, err
		}
		return false, nil
	}

	charged := false
	err = s.db.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		n, uerr := q.ApplyInvoicePaymentSnapshot(ctx, gen.ApplyInvoicePaymentSnapshotParams{
			MerchantID: r.MerchantID, CustomerID: r.CustomerID, InvoiceID: r.InvoiceID,
			Snapshot: snapshot, Now: now,
		})
		if uerr != nil {
			return uerr
		}
		if n == 0 {
			return nil
		}
		if externalInvoiceID := optionalString(res.ExternalInvoiceID); externalInvoiceID != nil {
			if _, err := q.SetInvoiceExternalID(ctx, gen.SetInvoiceExternalIDParams{
				MerchantID:        r.MerchantID,
				CustomerID:        r.CustomerID,
				InvoiceID:         r.InvoiceID,
				ExternalInvoiceID: externalInvoiceID,
				Now:               now,
			}); err != nil {
				return err
			}
		}
		if _, err := q.ReduceMoneyOutstandingOwedSnapshot(ctx, gen.ReduceMoneyOutstandingOwedSnapshotParams{
			MerchantID: r.MerchantID, CustomerID: r.CustomerID, Currency: normalizeCurrency(r.Currency),
			Snapshot: snapshot, Now: now,
		}); err != nil {
			return err
		}
		sid := key
		trx := &models.MoneyTransaction{
			ID: uuidutil.NewV7(), MerchantID: r.MerchantID, CustomerID: r.CustomerID, Currency: normalizeCurrency(r.Currency), Invoker: r.CustomerID.String(),
			Amount: -snapshot, TransactionType: txOwedPayment, Status: "posted",
			Source: "invoice_charge", SourceID: &sid, InvoiceID: &r.InvoiceID, CreatedAt: now, UpdatedAt: now,
		}
		if err := q.InsertMoneyTransactionIfAbsent(ctx, gen.InsertMoneyTransactionIfAbsentParams(insertParamsFromTransaction(trx))); err != nil {
			return err
		}
		storedTrx, err := q.GetMoneyTransactionByCoords(ctx, gen.GetMoneyTransactionByCoordsParams{
			MerchantID:      r.MerchantID,
			CustomerID:      r.CustomerID,
			Currency:        normalizeCurrency(r.Currency),
			TransactionType: txOwedPayment,
			Source:          "invoice_charge",
			SourceID:        &sid,
		})
		if err != nil {
			return err
		}
		processor := optionalProcessor(res.Processor)
		processorPaymentID := optionalString(res.TransactionID)
		if err := q.InsertInvoicePayment(ctx, gen.InsertInvoicePaymentParams{
			ID:                 uuidutil.NewV7(),
			MerchantID:         r.MerchantID,
			CustomerID:         r.CustomerID,
			InvoiceID:          r.InvoiceID,
			MoneyTransactionID: &storedTrx.ID,
			Currency:           normalizeCurrency(r.Currency),
			Amount:             snapshot,
			Status:             "settled",
			Processor:          processor,
			ProcessorPaymentID: processorPaymentID,
			AttemptedAt:        now,
			SettledAt:          &now,
			CreatedAt:          now,
			UpdatedAt:          now,
		}); err != nil {
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

func (s *MoneyService) recordInvoicePaymentAttempt(ctx context.Context, r invoiceArrearsAccount, trxID *uuid.UUID, status string, processor, processorPaymentID, failureCode, failureMessage *string, now time.Time) error {
	return s.db.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return gen.New(tx).InsertInvoicePayment(ctx, gen.InsertInvoicePaymentParams{
			ID:                 uuidutil.NewV7(),
			MerchantID:         r.MerchantID,
			CustomerID:         r.CustomerID,
			InvoiceID:          r.InvoiceID,
			MoneyTransactionID: trxID,
			Currency:           normalizeCurrency(r.Currency),
			Amount:             r.AmountDue,
			Status:             status,
			Processor:          processor,
			ProcessorPaymentID: processorPaymentID,
			FailureCode:        failureCode,
			FailureMessage:     failureMessage,
			AttemptedAt:        now,
			CreatedAt:          now,
			UpdatedAt:          now,
		})
	})
}

func optionalProcessor(processor string) *string {
	processor = normalizeProcessor(processor)
	if processor == "" {
		return nil
	}
	return &processor
}

func optionalString(v string) *string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	return &v
}

func (s *MoneyService) chargeOneOutstanding(ctx context.Context, charger Charger, r arrearsAccount) (bool, error) {
	payer := identity.CustomerID(r.CustomerID)
	snapshot := r.Owed
	if snapshot <= 0 || r.PaymentMethodID == nil {
		return false, nil
	}
	// #474 invariant: never charge a card for a custom-credit (non-currency) balance.
	if err := RequireBillingCurrency(normalizeCurrency(r.Currency)); err != nil {
		return false, nil
	}
	key := fmt.Sprintf("arrears:%s:%d", r.CustomerID, snapshot)

	// Owed is in the ledger currency's internal precision; the processor charges whole cents
	// (1 cent = 10,000 internal units). Round up so we never under-collect.
	res, err := charger.ChargeSavedMethod(ctx, ChargeRequest{
		MerchantID:      r.MerchantID,
		Payer:           payer,
		Invoker:         payer.UUID().String(),
		PaymentMethodID: *r.PaymentMethodID,
		AmountCents:     (snapshot + 9_999) / 10_000,
		Currency:        normalizeCurrency(r.Currency),
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
			MerchantID: r.MerchantID, CustomerID: r.CustomerID, Currency: normalizeCurrency(r.Currency),
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
			ID: uuidutil.NewV7(), MerchantID: r.MerchantID, CustomerID: r.CustomerID, Currency: normalizeCurrency(r.Currency), Invoker: r.CustomerID.String(),
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
