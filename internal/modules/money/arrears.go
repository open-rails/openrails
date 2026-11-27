package money

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/modules/money/ledger"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money/books"
)

// Arrears transaction types (postpaid usage ledger).
const (
	txOwedAccrual   = "owed_accrual"   // usage accrued to outstanding owed (positive amount)
	txOwedPayment   = "owed_payment"   // owed collected via a card charge (negative amount)
	txOwedRepayment = "owed_repayment" // owed repaid from newly funded credit (negative amount)
)

// AccrueOwed records postpaid usage against an arrears account. It writes the
// durable usage ledger row and a pending invoice item; issued debt exists only
// once those pending items are finalized onto an open invoice. Idempotent on
// (customer, source, source_id).
func (s *MoneyService) AccrueOwed(ctx context.Context, customer identity.CustomerID, currency, source, sourceID string, amount int64) (*models.MoneyTransaction, error) {
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
	customerID := customer.UUID()
	now := s.now()

	var trx *models.MoneyTransaction
	// MerchantTx pins the context's merchant for the transaction, so this works
	// off the request path too.
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)

		// Take the per-customer spend lock (lockBalance's customers row FOR
		// UPDATE) before the check-then-insert, so concurrent accruals at the
		// same coordinates serialize: one posts, the other replays.
		if err := ensureCustomer(ctx, q, tenantID, customerID); err != nil {
			return err
		}
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{
			ID: customerID, MerchantID: tenantID,
		}); err != nil {
			return err
		}

		// Idempotency: an owed-accrual transfer at these coordinates means it ran.
		existing, gerr := q.GetLedgerTransferByCoords(ctx, gen.GetLedgerTransferByCoordsParams{
			MerchantID: tenantID, CustomerID: customerID, Currency: cur,
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

		if err := s.ensureSettingsRowTx(ctx, q, tenantID, customerID, cur, BillingModeArrears, now); err != nil {
			return err
		}

		ml := s.moneyLedger(q, tenantID)
		tr, err := ml.AccrueOwed(ctx, customerID, cur, amount, key.Coord(), nil)
		if err != nil {
			return err
		}
		if err := books.New(q, tenantID).AccrueUsage(ctx, customerID, cur, amount, key.Coord(), now, nil); err != nil {
			return err
		}
		trx = moneyTransactionFromTransfer(tr)
		if err := insertPendingOwedItemTx(ctx, q, tenantID, customerID, cur, txOwedAccrual, key.owedItemSourceID(), amount, now, map[string]any{
			"operation": string(key.Operation()),
			"source":    key.Source(),
		}); err != nil {
			return err
		}
		return s.WithDatabase(s.db.NewWithPgxTx(tx)).queueUsageBilling(ctx, q, customerID, cur)
	})
	if err != nil {
		return nil, err
	}
	return trx, nil
}

// ensureSettingsRowTx inserts a default settings row for customer if one does not
// exist, using the given billing mode. No-op when the row exists.
func (s *MoneyService) ensureSettingsRowTx(ctx context.Context, q *gen.Queries, tenantID, customerID uuid.UUID, currency, mode string, now time.Time) error {
	// money_settings references the customers row; this is the shared choke
	// point for settings writes.
	if err := ensureCustomer(ctx, q, tenantID, customerID); err != nil {
		return err
	}
	inserted, err := q.InsertMoneyAccountSettingsIfAbsent(ctx, gen.InsertMoneyAccountSettingsIfAbsentParams{
		MerchantID: tenantID, CustomerID: customerID, Currency: normalizeCurrency(currency),
		BillingMode: mode, Now: now,
	})
	if err != nil || inserted == 0 || mode != BillingModeArrears {
		return err
	}
	// No row was prepaid: an arrears customer's balance is not watched.
	return ledger.WatchCustomerBalance(ctx, q, gen.WatchCustomerBalanceParams{MerchantID: tenantID, CustomerID: customerID, Currency: normalizeCurrency(currency), Now: now})
}

// GetOutstandingOwed returns the customer's arrears exposure in currency, read
// O(1) from their arrears-liability account. The ledger, not invoices, is the
// exposure source: every invoice line already has an owed_accrual leg.
func (s *MoneyService) GetOutstandingOwed(ctx context.Context, customer identity.CustomerID, currency string) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("money service not initialized")
	}
	if customer.IsZero() {
		return 0, fmt.Errorf("customer required")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	cur := normalizeCurrency(currency)
	if IsMerchantUnit(cur) {
		return 0, nil // nothing is owed in a merchant's unit
	}
	if err := RequireBillingCurrency(cur); err != nil {
		return 0, err
	}
	var owed int64
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var e error
		owed, e = s.moneyLedger(s.db.Gen(ctx), tid.UUID()).OutstandingOwed(ctx, customer.UUID(), cur)
		return e
	})
	return owed, err
}

func optionalString(v string) *string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	return &v
}
