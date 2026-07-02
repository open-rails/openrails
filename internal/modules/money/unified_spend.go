package money

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Unified credit-line model. An account spends prepaid balance first, then
// creates pending invoice items up to its admin-set credit_limit_amount.
// Prepay-only = credit limit 0; arrears = explicit credit line.

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
// gated by the credit line. Idempotent on (merchant, payer, currency, source,
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

	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
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
			existing, cerr := q.CountLedgerSpendByCoords(ctx, gen.CountLedgerSpendByCoordsParams{
				MerchantID: tenantID, CustomerID: payer.UUID(), Currency: cur,
				Source: params.Source, SourceID: params.SourceID,
			})
			if cerr != nil {
				return cerr
			}
			if existing > 0 {
				return nil // already spent; idempotent no-op
			}
		}

		_, _, serr := s.spendBalanceThenOwedTx(ctx, q, payer, params.Invoker, cur, params.Source, params.SourceID, params.Amount, false)
		return serr
	})
}

// CaptureAuthorized records the durable money movement for an admitted request.
// The in-flight authorization itself lives in Redis; this method only posts the
// actual charge and is idempotent on (merchant, payer, currency, source,
// source_id).
//
// #513 decision 8: capture RECORDS REALITY UNCONDITIONALLY. The Redis spendgate
// at admit time is the ONLY gate, and concurrent admits may bounded-over-admit.
// Capture therefore must NOT re-gate the credit line: it draws the prepaid
// balance first and records any remainder as owed/overdraft (even for a prepaid
// payer with no credit line — an involuntary overdraft that is reconciled later).
// Re-gating here would mean "served but can't charge", which is deterministic on
// idempotent retry and a hard failure for the caller.
func (s *MoneyService) CaptureAuthorized(ctx context.Context, params SpendParams) (*models.MoneyTransaction, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if params.Amount <= 0 {
		return nil, fmt.Errorf("amount must be positive")
	}
	params.Source = strings.TrimSpace(params.Source)
	params.SourceID = strings.TrimSpace(params.SourceID)
	if params.Source == "" || params.SourceID == "" {
		return nil, fmt.Errorf("source and source_id required")
	}
	cur := normalizeCurrency(params.Currency)
	if err := ValidateCurrency(cur); err != nil {
		return nil, err
	}
	payer, err := resolveCustomer(params.Payer, params.Invoker)
	if err != nil {
		return nil, err
	}

	var trx *models.MoneyTransaction
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		tid, terr := merchant.Require(ctx)
		if terr != nil {
			return terr
		}
		tenantID := tid.UUID()

		if _, err := s.lockBalance(ctx, q, payer, params.Invoker, cur); err != nil {
			return err
		}
		existing, gerr := q.GetLedgerSpendByCoords(ctx, gen.GetLedgerSpendByCoordsParams{
			MerchantID: tenantID,
			CustomerID: payer.UUID(),
			Currency:   cur,
			Source:     params.Source,
			SourceID:   params.SourceID,
		})
		if gerr == nil {
			trx = moneyTransactionFromTransfer(existing)
			return nil
		}
		if !errors.Is(gerr, pgx.ErrNoRows) {
			return gerr
		}

		if _, _, serr := s.spendBalanceThenOwedTx(ctx, q, payer, params.Invoker, cur, params.Source, params.SourceID, params.Amount, true); serr != nil {
			return serr
		}
		// The durable spend is the first transfer at these coordinates (the
		// balance debit, else the owed accrual).
		row, rerr := q.GetLedgerSpendByCoords(ctx, gen.GetLedgerSpendByCoordsParams{
			MerchantID: tenantID, CustomerID: payer.UUID(), Currency: cur,
			Source: params.Source, SourceID: params.SourceID,
		})
		if rerr != nil {
			if errors.Is(rerr, pgx.ErrNoRows) {
				return fmt.Errorf("capture produced no money transfer")
			}
			return rerr
		}
		trx = moneyTransactionFromTransfer(row)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return trx, nil
}

// spendBalanceThenOwedTx debits `amount` within an existing tx: it draws the
// prepaid balance first (FIFO credit lots → #512 ledger spend transfers) and
// accrues any remainder to pending invoice items + an arrears-liability ledger
// transfer. The caller must have already locked the balance row (serialization
// point) and handled idempotency. Returns the amounts drawn from balance and
// accrued to owed (either may be 0).
//
// preAuthorized selects the gating contract:
//   - false (immediate SpendCredits): the remainder is GATED by the account's
//     credit line — a prepaid payer (no/zero credit line) or a spend that would
//     push owed past the line is rejected with ErrInsufficientCredits, and
//     nothing is debited.
//   - true (capture of a pre-authorized hold, #513 decision 8): NEVER re-gates.
//     The remainder is recorded as owed/overdraft unconditionally — including for
//     a prepaid payer with no credit line (an involuntary overdraft). The Redis
//     spendgate at admit time was the only gate; capture records reality.
func (s *MoneyService) spendBalanceThenOwedTx(
	ctx context.Context, q *gen.Queries, payer identity.CustomerID,
	userID, currency, source, sourceID string, amount int64, preAuthorized bool,
) (balanceSpent, owedAccrued int64, err error) {
	if amount <= 0 {
		return 0, 0, fmt.Errorf("amount must be positive")
	}
	now := s.now()
	cur := normalizeCurrency(currency)
	tid, err := merchant.Require(ctx)
	if err != nil {
		return 0, 0, err
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()

	bal, err := s.lockBalance(ctx, q, payer, userID, cur)
	if err != nil {
		return 0, 0, err
	}
	available := bal.Balance - bal.HeldBalance
	if available < 0 {
		available = 0
	}
	// bal.Balance is the raw ledger balance and can include unswept lapsed-lot
	// remainders that CreditSpend cannot draw (#676): cap the balance leg at the
	// spendable-lot total so a lapsed lot never fails the spend outright.
	lots, lerr := q.ListSpendableCreditLots(ctx, gen.ListSpendableCreditLotsParams{
		MerchantID: tenantID, CustomerID: payerID, Currency: cur, AsOf: now,
	})
	if lerr != nil {
		return 0, 0, lerr
	}
	var spendable int64
	for _, lot := range lots {
		if lot.Remaining > 0 {
			spendable += lot.Remaining
		}
	}
	if available > spendable {
		available = spendable
	}
	fromBalance := amount
	if fromBalance > available {
		fromBalance = available
	}
	fromOwed := amount - fromBalance

	if fromOwed > 0 {
		if preAuthorized {
			// Pre-authorized capture never re-gates: the remainder becomes
			// owed/overdraft regardless of the credit line. Ensure a settings row
			// exists so the owed accrual has a home (mirrors the arrears flow).
			if err := s.ensureSettingsRowTx(ctx, q, tenantID, payerID, cur, BillingModeArrears, now); err != nil {
				return 0, 0, err
			}
		} else {
			// Immediate spend: the remainder can only go to owed when the account
			// has a credit line, and only up to that line.
			settingsRow, serr := q.LockMoneyAccountSettings(ctx, gen.LockMoneyAccountSettingsParams{
				MerchantID: tenantID, CustomerID: payerID, Currency: cur,
			})
			if errors.Is(serr, pgx.ErrNoRows) {
				// No settings row => prepaid default => no credit line.
				return 0, 0, ErrInsufficientCredits
			}
			if serr != nil {
				return 0, 0, serr
			}
			settings := settingsFromGen(settingsRow)
			if settings.BillingMode != BillingModeArrears {
				return 0, 0, ErrInsufficientCredits // prepay-only: credit limit 0
			}
			if settings.CreditLimitAmount <= 0 {
				return 0, 0, ErrInsufficientCredits
			}
			exposure, eerr := s.arrearsExposureTx(ctx, q, tenantID, payerID, cur)
			if eerr != nil {
				return 0, 0, eerr
			}
			if exposure+fromOwed > settings.CreditLimitAmount {
				return 0, 0, ErrInsufficientCredits // would exceed the credit line
			}
		}
	}

	if fromBalance > 0 {
		if _, err := s.withdrawBalanceAndBlocks(ctx, q, payer, userID, cur, source, sourceID, "", fromBalance); err != nil {
			return 0, 0, err
		}
		balanceSpent = fromBalance
	}

	if fromOwed > 0 {
		ml := s.moneyLedger(q, tenantID)
		if _, err := ml.AccrueOwed(ctx, payerID, cur, fromOwed, source, sourceID, nil); err != nil {
			return 0, 0, err
		}
		itemSourceID := sourceID
		if itemSourceID == "" {
			itemSourceID = uuidutil.NewV7().String()
		}
		if err := insertPendingInvoiceItemTx(ctx, q, tenantID, payerID, cur, txOwedAccrual, source+":"+itemSourceID, nil, fromOwed, now, map[string]any{
			"source": source,
		}); err != nil {
			return 0, 0, err
		}
		owedAccrued = fromOwed
	}

	return balanceSpent, owedAccrued, nil
}
