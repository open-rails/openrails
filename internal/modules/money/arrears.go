package money

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
)

// Arrears transaction types (postpaid usage ledger, issue #241).
const (
	txOwedAccrual   = "owed_accrual"   // usage accrued to outstanding owed (positive amount)
	txOwedPayment   = "owed_payment"   // owed collected via a card charge (negative amount)
	txOwedRepayment = "owed_repayment" // owed repaid from newly funded credit (negative amount)
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
	// Owed/arrears requires a registered currency.
	cur := normalizeCurrency(currency)
	if err := RequireBillingCurrency(cur); err != nil {
		return nil, err
	}
	key, kerr := NewIdempotencyKey(OpArrearsAccrual, source, sourceID)
	if kerr != nil {
		return nil, kerr
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()
	now := s.now()

	var trx *models.MoneyTransaction
	// or#868 B2: this was a bare RunInTx under the comment "privileged (no-GUC)
	// transaction". No such pool exists — it worked only where an HTTP request
	// had already pinned a merchant connection and pgxBegin inherited its GUC.
	// Off that path (internal/service.FinalizeInvoice, MoneyService.SweepUsage, both
	// embedded seams) the transaction carried no openrails.merchant_id and the
	// since-removed RLS denied every insert below (42501), so metered/arrears
	// billing was inoperable there. MerchantTx sets the GUC transaction-locally
	// from the context's merchant, which the explicit merchant_id predicates
	// then agree with.
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
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
			TransferType: "owed_accrual", Operation: string(key.Operation()),
			Source: key.Source(), SourceID: key.SourceID(),
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
		tr, err := ml.AccrueOwed(ctx, payerID, cur, amount, key.Coord(), nil)
		if err != nil {
			return err
		}
		trx = moneyTransactionFromTransfer(tr)
		return insertPendingInvoiceItemTx(ctx, q, tenantID, payerID, cur, txOwedAccrual, key.invoiceItemSourceID(), amount, now, map[string]any{
			"operation": string(key.Operation()),
			"source":    key.Source(),
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

// GetOutstandingOwed returns the payer's current arrears exposure in currency,
// read O(1) from their arrears-liability account (or#897).
//
// It was invoice-derived (open invoices + pending items). Invoices are
// presentation/collection artifacts that lag the ledger by a finalize cycle,
// and EVERY invoice line already has an owed_accrual leg — verified: the only
// pending-item writer is insertPendingInvoiceItemTx and all of its callers post
// an accrual first — so the invoice view could only ever be a staler copy of
// the ledger. One substrate, and it is the ledger.
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
	// or#868 B2: still pinned (under the since-removed RLS an unpinned read saw
	// no account and reported zero exposure, which is fail-OPEN).
	var owed int64
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var e error
		owed, e = s.moneyLedger(s.db.Gen(ctx), tid.UUID()).OutstandingOwed(ctx, payer.UUID(), cur)
		return e
	})
	return owed, err
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
