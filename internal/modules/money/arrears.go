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
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
	log "github.com/sirupsen/logrus"
)

// Arrears transaction types (postpaid usage ledger, issue #241).
const (
	txOwedAccrual = "owed_accrual" // usage accrued to outstanding owed (positive amount)
	txOwedPayment = "owed_payment" // owed collected via a card charge (negative amount)
)

// AccrueOwed records postpaid usage against an arrears account. It writes the
// durable usage ledger row and a pending invoice item; issued debt exists only
// once those pending items are finalized onto an open invoice. Idempotent on
// (payer, source, source_id).
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

		// #677: per-customer spend mutex (same customers-row FOR UPDATE as
		// lockBalance) BEFORE the check-then-insert, so concurrent accruals at the
		// same coordinates serialize — one posts, the other replays it below.
		if err := ensureCustomer(ctx, q, tenantID, payerID); err != nil {
			return err
		}
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{
			ID: payerID, MerchantID: tenantID,
		}); err != nil {
			return err
		}

		// Idempotency: an owed-accrual transfer at these coordinates means it ran.
		existing, gerr := q.GetLedgerTransferByCoords(ctx, gen.GetLedgerTransferByCoordsParams{
			MerchantID: tenantID, CustomerID: payerID, Currency: cur,
			TransferType: "owed_accrual", Source: source, SourceID: sourceID,
		})
		if gerr == nil {
			trx = moneyTransactionFromTransfer(existing)
			return nil
		}
		if !errors.Is(gerr, pgx.ErrNoRows) {
			return gerr
		}

		if err := s.ensureSettingsRowTx(ctx, q, tenantID, payerID, cur, BillingModeArrears, now); err != nil {
			return err
		}

		ml := s.moneyLedger(q, tenantID)
		tr, err := ml.AccrueOwed(ctx, payerID, cur, amount, source, sourceID, nil)
		if err != nil {
			return err
		}
		trx = moneyTransactionFromTransfer(tr)
		return insertPendingInvoiceItemTx(ctx, q, tenantID, payerID, cur, txOwedAccrual, source+":"+sourceID, amount, now, map[string]any{
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
		MerchantID: tenantID, CustomerID: payerID, Currency: normalizeCurrency(currency),
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

// GetOutstandingOwed returns current arrears exposure for payer in currency:
// pending unbilled invoice items plus open/past-due invoice balances. The name
// is retained for older callers, but the value is invoice-derived.
func (s *MoneyService) GetOutstandingOwed(ctx context.Context, payer identity.CustomerID, currency string) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return 0, fmt.Errorf("payer required")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	cur := normalizeCurrency(currency)
	if err := RequireBillingCurrency(cur); err != nil {
		return 0, err
	}
	return s.arrearsExposureTx(ctx, s.db.Gen(ctx), tid.UUID(), payer.UUID(), cur)
}

type invoiceArrearsAccount struct {
	InvoiceID       uuid.UUID
	MerchantID      uuid.UUID
	CustomerID      uuid.UUID
	Currency        string
	AmountDue       int64
	FailureCount    int32
	FirstFailureAt  *time.Time
	PaymentMethodID *uuid.UUID
}

type invoiceCollectionClaim struct {
	account        invoiceArrearsAccount
	previous       *models.Invoice
	attemptID      uuid.UUID
	idempotencyKey string
}

// ChargeOutstanding collects open/past-due invoice receivables by charging the
// saved payment method on file. When minThreshold > 0 only invoices due at least
// that much are charged; minThreshold <= 0 charges every open invoice due.
// Returns the number of successful invoice charges. (#241/#506)
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
	if err := s.expireStaleInvoiceCollectionRetryClaims(ctx, tenantID, s.now()); err != nil {
		return 0, err
	}
	invoiceRows, err := s.db.Gen(ctx).ListChargeableOpenInvoices(ctx, gen.ListChargeableOpenInvoicesParams{
		MerchantID: tenantID, Now: s.now(), MinThreshold: minThreshold,
	})
	if err != nil {
		return 0, err
	}
	count := 0
	for _, r := range invoiceRows {
		claim, ok, err := s.claimInvoiceCollection(ctx, identity.CustomerID(r.CustomerID), r.ID, minThreshold, false, s.now())
		if err != nil {
			return count, err
		}
		if !ok {
			continue
		}
		charged, err := s.chargeClaimedInvoice(ctx, charger, claim)
		if err != nil {
			return count, err
		}
		if charged {
			count++
		}
	}
	return count, nil
}

func (s *MoneyService) arrearsExposureTx(ctx context.Context, q *gen.Queries, merchantID, customerID uuid.UUID, currency string) (int64, error) {
	openDue, err := q.SumOpenInvoiceAmountDue(ctx, gen.SumOpenInvoiceAmountDueParams{
		MerchantID: merchantID, CustomerID: customerID, Currency: normalizeCurrency(currency),
	})
	if err != nil {
		return 0, err
	}
	pendingDue, err := q.SumPendingInvoiceItemAmount(ctx, gen.SumPendingInvoiceItemAmountParams{
		MerchantID: merchantID, CustomerID: customerID, Currency: normalizeCurrency(currency),
	})
	if err != nil {
		return 0, err
	}
	return openDue + pendingDue, nil
}

func (s *MoneyService) chargeClaimedInvoice(ctx context.Context, charger Charger, claim *invoiceCollectionClaim) (bool, error) {
	if claim == nil {
		return false, fmt.Errorf("invoice collection claim required")
	}
	r := claim.account
	payer := identity.CustomerID(r.CustomerID)
	snapshot := r.AmountDue
	if snapshot <= 0 || r.PaymentMethodID == nil {
		return false, nil
	}
	if err := RequireBillingCurrency(normalizeCurrency(r.Currency)); err != nil {
		return false, nil
	}
	key := claim.idempotencyKey
	chargeMinor, err := NativeToRailMinor(r.Currency, snapshot)
	if err != nil {
		cleanupCtx, cancel := collectionCleanupContext(ctx)
		defer cancel()
		return false, errors.Join(err, s.releaseInvoiceCollectionClaim(cleanupCtx, claim, s.now()))
	}
	res, err := charger.ChargeSavedMethod(ctx, ChargeRequest{
		MerchantID:      r.MerchantID,
		Payer:           payer,
		Invoker:         payer.UUID().String(),
		InvoiceID:       &r.InvoiceID,
		PaymentMethodID: *r.PaymentMethodID,
		AmountCents:     chargeMinor,
		Currency:        normalizeCurrency(r.Currency),
		IdempotencyKey:  key,
		Description:     fmt.Sprintf("invoice %s", r.InvoiceID.String()),
	})
	if err != nil {
		cleanupCtx, cancel := collectionCleanupContext(ctx)
		defer cancel()
		if isCollectionOutcomeAmbiguous(err) {
			if recordErr := s.markInvoiceCollectionOutcomeUnknown(cleanupCtx, claim, s.now()); recordErr != nil {
				return false, errors.Join(err, recordErr)
			}
			return false, err
		}
		if releaseErr := s.releaseInvoiceCollectionClaim(cleanupCtx, claim, s.now()); releaseErr != nil {
			return false, errors.Join(err, releaseErr)
		}
		return false, err
	}
	now := s.now()
	cleanupCtx, cancel := collectionCleanupContext(ctx)
	defer cancel()
	if res.Declined {
		action := invoiceCollectionAction(res.Rail, res.FailureCode, r.FailureCount, r.FirstFailureAt, now)
		if err := s.recordInvoiceCollectionFailure(cleanupCtx, claim, optionalRail(res.Rail), optionalString(res.TransactionID), res.FailureCode, res.FailureMessage, action, now); err != nil {
			markCtx, markCancel := collectionCleanupContext(ctx)
			defer markCancel()
			if markErr := s.markInvoiceCollectionOutcomeUnknown(markCtx, claim, s.now()); markErr != nil {
				return false, errors.Join(err, markErr)
			}
			return false, err
		}
		return false, nil
	}

	charged := false
	err = s.db.MerchantTx(cleanupCtx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		n, uerr := q.ApplyInvoicePaymentSnapshot(ctx, gen.ApplyInvoicePaymentSnapshotParams{
			MerchantID: r.MerchantID, CustomerID: r.CustomerID, InvoiceID: r.InvoiceID,
			Snapshot: snapshot, Now: now,
		})
		if uerr != nil {
			return uerr
		}
		// n == 0: the invoice mutated under us AFTER a successful provider charge
		// (paid/voided/partially settled elsewhere). NEVER drop the charge (#674):
		// unless this key was already recorded, record the settled payment below —
		// unapplied to amount_due — and alert for repair.
		applied := n > 0
		if !applied {
			if _, err := q.MarkInvoiceCollectionOutcomeUnknown(ctx, gen.MarkInvoiceCollectionOutcomeUnknownParams{
				MerchantID: r.MerchantID,
				CustomerID: r.CustomerID,
				InvoiceID:  r.InvoiceID,
				Now:        now,
			}); err != nil {
				return err
			}
		}
		if applied {
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
		}
		sid := key
		cur := normalizeCurrency(r.Currency)
		ml := s.moneyLedger(q, r.MerchantID)
		// Settle the arrears liability via the external charge. An existing
		// owed-payment transfer at this attempt key means a previous/concurrent run
		// already recorded this exact charge — done.
		storedTrx, err := q.GetLedgerTransferByCoords(ctx, gen.GetLedgerTransferByCoordsParams{
			MerchantID: r.MerchantID, CustomerID: r.CustomerID, Currency: cur,
			TransferType: "owed_payment", Source: "invoice_charge", SourceID: sid,
		})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if errors.Is(err, pgx.ErrNoRows) {
			storedTrx, err = ml.PayOwed(ctx, r.CustomerID, cur, snapshot, "invoice_charge", sid, &r.InvoiceID)
			if err != nil {
				return err
			}
		}
		rail := optionalRail(res.Rail)
		railPaymentID := optionalString(res.TransactionID)
		updated, err := q.SettleClaimedInvoicePaymentAttempt(ctx, gen.SettleClaimedInvoicePaymentAttemptParams{
			MerchantID:       r.MerchantID,
			CustomerID:       r.CustomerID,
			InvoiceID:        r.InvoiceID,
			AttemptID:        claim.attemptID,
			LedgerTransferID: &storedTrx.ID,
			Rail:             rail,
			RailPaymentID:    railPaymentID,
			Now:              now,
		})
		if err != nil {
			return err
		}
		if updated != 1 {
			return fmt.Errorf("settle claimed invoice payment attempt: claim lost")
		}
		charged = true
		if !applied {
			log.WithContext(ctx).WithFields(log.Fields{
				"invoice_id": r.InvoiceID, "customer_id": r.CustomerID, "amount": snapshot, "rail_payment_id": derefStr(railPaymentID),
			}).Error("invoice charge succeeded but invoice snapshot no longer applies; payment recorded UNAPPLIED — needs repair (refund or manual allocation)")
		}
		return nil
	})
	if err != nil {
		markCtx, markCancel := collectionCleanupContext(ctx)
		defer markCancel()
		if markErr := s.markInvoiceCollectionOutcomeUnknown(markCtx, claim, s.now()); markErr != nil {
			return false, errors.Join(err, markErr)
		}
		return false, err
	}
	return charged, nil
}

func (s *MoneyService) recordInvoiceCollectionFailure(ctx context.Context, claim *invoiceCollectionClaim, rail, railPaymentID, failureCode, failureMessage *string, action invoiceCollectionFailureAction, now time.Time) error {
	r := claim.account
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		failureReason := payments.NormalizeFailureReason(derefStr(rail), derefStr(failureCode))
		attemptUpdated, err := q.FailClaimedInvoicePaymentAttempt(ctx, gen.FailClaimedInvoicePaymentAttemptParams{
			MerchantID:     r.MerchantID,
			CustomerID:     r.CustomerID,
			InvoiceID:      r.InvoiceID,
			AttemptID:      claim.attemptID,
			Rail:           rail,
			RailPaymentID:  railPaymentID,
			FailureCode:    failureCode,
			FailureReason:  &failureReason,
			FailureMessage: failureMessage,
			Now:            now,
		})
		if err != nil {
			return err
		}
		if attemptUpdated != 1 {
			return fmt.Errorf("fail claimed invoice payment attempt: claim lost")
		}
		updated, err := q.RecordInvoiceCollectionFailure(ctx, gen.RecordInvoiceCollectionFailureParams{
			MerchantID:           r.MerchantID,
			CustomerID:           r.CustomerID,
			InvoiceID:            r.InvoiceID,
			ExpectedFailureCount: r.FailureCount,
			Terminal:             action.terminal,
			NextAttemptAt:        action.nextAttemptAt,
			FailureCode:          failureCode,
			FailureMessage:       failureMessage,
			Now:                  now,
		})
		if err != nil {
			return err
		}
		if updated != 1 {
			return fmt.Errorf("record invoice collection failure: invoice claim lost")
		}
		return nil
	})
}

func (s *MoneyService) expireStaleInvoiceCollectionRetryClaims(ctx context.Context, merchantID uuid.UUID, now time.Time) error {
	_, err := s.db.Gen(ctx).ExpireStaleInvoiceCollectionClaims(ctx, gen.ExpireStaleInvoiceCollectionClaimsParams{
		MerchantID:  merchantID,
		Now:         now,
		StaleBefore: now.Add(-15 * time.Minute),
	})
	if err != nil {
		return fmt.Errorf("expire stale invoice collection claims: %w", err)
	}
	return nil
}

func optionalRail(rail string) *string {
	rail = normalizeRail(rail)
	if rail == "" {
		return nil
	}
	return &rail
}

func optionalString(v string) *string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	return &v
}
