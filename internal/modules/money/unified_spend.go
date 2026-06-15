package money

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Unified credit-line model (issue #302). Credit is ONE dial: an account spends
// its prepaid balance FIRST, then accrues to outstanding_owed up to its credit
// limit. Prepay-only = credit limit 0 (cannot go negative). Arrears = credit
// limit > 0 (a credit line). The limit is BillingMode + MaxOutstandingOwedAmount:
// prepaid -> 0; arrears -> MaxOutstandingOwedAmount (nil = unlimited line).
//
// This unifies the two IMMEDIATE-debit paths (prepaid Withdraw + arrears
// AccrueOwed) into one balance-first-then-owed spend. The hold->capture path and
// making BillingMode a pure display label are tracked as remaining #302 work.

// SpendParams is a unified immediate spend (balance first, then owed).
type SpendParams struct {
	Payer    *identity.CustomerID
	Invoker  string
	Currency string
	Amount   int64
	Source   string
	SourceID string
}

// SpendCredits debits an account balance-first-then-owed in one transaction,
// gated by the credit line. Idempotent on (tenant, payer, currency, source,
// source_id). Returns ErrInsufficientCredits when balance + remaining credit
// line cannot cover the amount.
func (s *MoneyService) SpendCredits(ctx context.Context, params SpendParams) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("money service not initialized")
	}
	if params.Amount <= 0 {
		return fmt.Errorf("amount must be positive")
	}
	params.Source = strings.TrimSpace(params.Source)
	params.SourceID = strings.TrimSpace(params.SourceID)
	cur := normalizeCurrency(params.Currency)
	if err := ValidateCurrency(cur); err != nil {
		return err
	}
	payer, err := resolveCustomer(params.Payer, params.Invoker)
	if err != nil {
		return err
	}

	return s.db.TenantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)

		// Serialize per account and guard idempotency on the spend coordinates.
		if _, err := s.lockBalance(ctx, q, payer, params.Invoker, cur); err != nil {
			return err
		}
		if params.SourceID != "" {
			tid, terr := merchant.Require(ctx)
			if terr != nil {
				return terr
			}
			tenantID := tid.UUID()
			existing, cerr := q.CountMoneySpendByCoords(ctx, gen.CountMoneySpendByCoordsParams{
				MerchantID: tenantID, CustomerID: payer.UUID(), Currency: cur,
				Source: params.Source, SourceID: &params.SourceID,
			})
			if cerr != nil {
				return cerr
			}
			if existing > 0 {
				return nil // already spent; idempotent no-op
			}
		}

		_, _, serr := s.spendBalanceThenOwedTx(ctx, q, payer, params.Invoker, cur, params.Source, params.SourceID, params.Amount)
		return serr
	})
}

// spendBalanceThenOwedTx debits `amount` within an existing tx: it draws the
// prepaid balance first (FIFO blocks) and accrues any remainder to
// outstanding_owed, gated by the account's credit line. The caller must have
// already locked the balance row (serialization point) and handled idempotency.
// Returns the balance-debit and owed-accrual transaction ids (either may be nil).
func (s *MoneyService) spendBalanceThenOwedTx(
	ctx context.Context, q *gen.Queries, payer identity.CustomerID,
	userID, currency, source, sourceID string, amount int64,
) (balanceTxnID, owedTxnID *uuid.UUID, err error) {
	if amount <= 0 {
		return nil, nil, fmt.Errorf("amount must be positive")
	}
	now := s.now()
	cur := normalizeCurrency(currency)
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, nil, err
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()

	bal, err := s.lockBalance(ctx, q, payer, userID, cur)
	if err != nil {
		return nil, nil, err
	}
	available := bal.Balance - bal.HeldBalance
	if available < 0 {
		available = 0
	}
	fromBalance := amount
	if fromBalance > available {
		fromBalance = available
	}
	fromOwed := amount - fromBalance

	// The remainder can only go to owed when the account has a credit line.
	if fromOwed > 0 {
		settingsRow, serr := q.LockMoneyAccountSettings(ctx, gen.LockMoneyAccountSettingsParams{
			MerchantID: tenantID, CustomerID: payerID, Currency: cur,
		})
		if errors.Is(serr, pgx.ErrNoRows) {
			// No settings row => prepaid default => no credit line.
			return nil, nil, ErrInsufficientCredits
		}
		if serr != nil {
			return nil, nil, serr
		}
		settings := settingsFromGen(settingsRow)
		if settings.BillingMode != BillingModeArrears {
			return nil, nil, ErrInsufficientCredits // prepay-only: credit limit 0
		}
		if settings.MaxOutstandingOwedAmount != nil &&
			settings.OutstandingOwedAmount+fromOwed > *settings.MaxOutstandingOwedAmount {
			return nil, nil, ErrInsufficientCredits // would exceed the credit line
		}
	}

	if fromBalance > 0 {
		if _, err := s.withdrawBalanceAndBlocks(ctx, q, payer, userID, cur, fromBalance); err != nil {
			return nil, nil, err
		}
		newBal := bal.Balance - fromBalance
		neg := -fromBalance
		trx := &models.MoneyTransaction{
			ID: uuidutil.NewV7(), MerchantID: tenantID, CustomerID: payerID, Currency: cur, Invoker: userID,
			Amount: neg, BalanceAfter: &newBal,
			TransactionType: "withdrawal", Status: "posted",
			Source: source, SourceID: nullStr(sourceID), CreatedAt: now, UpdatedAt: now,
		}
		if err := q.InsertMoneyTransaction(ctx, insertParamsFromTransaction(trx)); err != nil {
			return nil, nil, err
		}
		balanceTxnID = &trx.ID
	}

	if fromOwed > 0 {
		if err := q.AddMoneyOutstandingOwed(ctx, gen.AddMoneyOutstandingOwedParams{
			MerchantID: tenantID, CustomerID: payerID, Currency: cur,
			Amount: fromOwed, Now: now,
		}); err != nil {
			return nil, nil, err
		}
		trx := &models.MoneyTransaction{
			ID: uuidutil.NewV7(), MerchantID: tenantID, CustomerID: payerID, Currency: cur, Invoker: userID,
			Amount: fromOwed, TransactionType: txOwedAccrual, Status: "posted",
			Source: source, SourceID: nullStr(sourceID), CreatedAt: now, UpdatedAt: now,
		}
		if err := q.InsertMoneyTransaction(ctx, insertParamsFromTransaction(trx)); err != nil {
			return nil, nil, err
		}
		owedTxnID = &trx.ID
	}

	return balanceTxnID, owedTxnID, nil
}

func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// captureSettleTx settles a captured hold's actual amount balance-first-then-owed
// (#302): it draws min(amount, availableAfter) from the prepaid balance/blocks
// and spills any remainder to outstanding_owed (the arrears credit line). This is
// what lets a hold placed against an arrears credit line capture even when the
// balance can't cover it. availableAfter is the available balance AFTER this
// hold's reservation has been released. The capture is pre-authorized, so this
// never re-gates. Returns the post-settlement balance.
func (s *MoneyService) captureSettleTx(ctx context.Context, q *gen.Queries, payer identity.CustomerID, userID, currency string, amount, availableAfter int64) (int64, error) {
	now := s.now()
	cur := normalizeCurrency(currency)
	tid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()
	if availableAfter < 0 {
		availableAfter = 0
	}
	fromBalance := amount
	if fromBalance > availableAfter {
		fromBalance = availableAfter
	}
	fromOwed := amount - fromBalance

	var newBal int64
	if fromBalance > 0 {
		nb, err := s.withdrawBalanceAndBlocks(ctx, q, payer, userID, cur, fromBalance)
		if err != nil {
			return 0, err
		}
		newBal = nb
	} else {
		// Nothing drawn from balance (full spill to owed): report the derived
		// spendable total unchanged (#491).
		b, err := s.deriveBalance(ctx, q, tenantID, payerID, cur)
		if err != nil {
			return 0, err
		}
		newBal = b.Balance
	}

	if fromOwed > 0 {
		// Only reachable for an arrears credit line (a prepaid hold reserves balance).
		if err := s.ensureSettingsRowTx(ctx, q, tenantID, payerID, cur, BillingModeArrears, now); err != nil {
			return 0, err
		}
		if err := q.AddMoneyOutstandingOwed(ctx, gen.AddMoneyOutstandingOwedParams{
			MerchantID: tenantID, CustomerID: payerID, Currency: cur,
			Amount: fromOwed, Now: now,
		}); err != nil {
			return 0, err
		}
	}
	return newBal, nil
}
