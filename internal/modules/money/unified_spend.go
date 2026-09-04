package money

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Unified credit-line model. An account spends prepaid balance first, then
// creates pending invoice items up to its admin-set credit_limit_amount.
// Prepay-only = credit limit 0; arrears = explicit credit line.

// SpendParams is a unified immediate spend (balance first, then owed). Key is
// built with money.NewIdempotencyKey — Source/SourceID are no longer two loose
// strings a caller can half-fill (or#892).
type SpendParams struct {
	Payer    *identity.CustomerID
	Invoker  string
	Currency string
	Amount   int64
	Key      IdempotencyKey
}

// SpendCredits debits an account balance-first-then-owed in one transaction,
// gated by the credit line. Idempotent on
// (merchant, payer, currency, operation=spend, source, source_id) — the key is
// REQUIRED, and a replay carrying a different Amount is refused with
// ErrIdempotencyKeyReused rather than answered with the first result (see
// idempotency.go). Returns ErrInsufficientCredits when balance + remaining
// credit line cannot cover the amount.
//
// The returned transaction carries Replayed (or#892): false = this call moved
// the money, true = the coordinate was already committed and nothing moved now.
func (s *MoneyService) SpendCredits(ctx context.Context, params SpendParams) (*models.MoneyTransaction, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if params.Amount <= 0 {
		return nil, fmt.Errorf("amount must be positive")
	}
	// or#891 item 1: this used to be `if params.SourceID != ""` around the dedupe
	// read only, so a keyless spend posted unconditionally and every retry
	// double-debited — while CaptureAuthorized, off this SAME struct, hard-required
	// both halves. That asymmetry was the defect, not caller sloppiness.
	if err := params.Key.RequireOperation(OpSpend); err != nil {
		return nil, err
	}
	cur := normalizeUnit(params.Currency)
	if err := s.validateUnit(ctx, cur); err != nil {
		return nil, err
	}
	payer, err := resolveCustomer(params.Payer, params.Invoker)
	if err != nil {
		return nil, err
	}

	var trx *models.MoneyTransaction
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)

		// Serialize per account and guard idempotency on the spend coordinates.
		if _, err := s.lockBalance(ctx, q, payer, params.Invoker, cur); err != nil {
			return err
		}
		tid, terr := merchant.Require(ctx)
		if terr != nil {
			return terr
		}
		tenantID := tid.UUID()
		committed, cerr := params.Key.requireSameAmount(ctx, q, tenantID, payer.UUID(), cur, params.Amount)
		if cerr != nil {
			return cerr
		}
		applied := false
		var serr error
		if !committed {
			// The pre-check is a fast path and a body guard, NOT the enforcement:
			// once-only is the unique index under spendBalanceThenOwedTx, so
			// `applied` stays authoritative when two transactions race past it.
			//
			// A REPLAY must not reach the spend arithmetic at all. The balance has
			// already moved, so re-deriving fromBalance/fromOwed against it splits
			// the same charge into a short balance leg plus an owed remainder and
			// denies a prepaid payer with ErrInsufficientCredits — a replay
			// answered with a hard failure, which is the exact shape #513
			// decision 8 forbids.
			_, _, applied, serr = s.spendBalanceThenOwedTx(ctx, q, payer, params.Invoker, cur, params.Key, params.Amount, false)
			if serr != nil {
				return serr
			}
		}
		row, rerr := q.GetLedgerSpendByCoords(ctx, gen.GetLedgerSpendByCoordsParams{
			MerchantID: tenantID, CustomerID: payer.UUID(), Currency: cur,
			Operation: string(params.Key.Operation()),
			Source:    params.Key.Source(), SourceID: params.Key.SourceID(),
		})
		if rerr != nil {
			return rerr
		}
		trx = moneyTransactionFromTransfer(row)
		trx.Replayed = !applied
		return nil
	})
	if err != nil {
		return nil, err
	}
	return trx, nil
}

// CaptureAuthorized records the durable money movement for an admitted request.
// The in-flight authorization itself lives in Redis; this method only posts the
// actual charge and is idempotent on (merchant, payer, currency, source,
// source_id).
//
// Custom units consume existing credits only; insufficient funding refuses the
// capture without creating debt. The overdraft policy below applies to ISO units.
//
// #513 decision 8: ISO capture RECORDS REALITY UNCONDITIONALLY. The Redis spendgate
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
	if err := params.Key.RequireOperation(OpCapture); err != nil {
		return nil, err
	}
	cur := normalizeUnit(params.Currency)
	if err := s.validateUnit(ctx, cur); err != nil {
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
		// or#891 item 3: a key hit used to return the first transfer WITHOUT
		// comparing Amount, so a retry that corrected the amount was told it
		// succeeded while the ledger kept the original number. Compare the total
		// already posted at these coordinates — not the first transfer's amount,
		// which is only one FIFO lot's take — and refuse a changed body.
		if _, cerr := params.Key.requireSameAmount(ctx, q, tenantID, payer.UUID(), cur, params.Amount); cerr != nil {
			return cerr
		}
		existing, gerr := q.GetLedgerSpendByCoords(ctx, gen.GetLedgerSpendByCoordsParams{
			MerchantID: tenantID,
			CustomerID: payer.UUID(),
			Currency:   cur,
			Operation:  string(params.Key.Operation()),
			Source:     params.Key.Source(),
			SourceID:   params.Key.SourceID(),
		})
		if gerr == nil {
			trx = moneyTransactionFromTransfer(existing)
			trx.Replayed = true
			return nil
		}
		if !errors.Is(gerr, pgx.ErrNoRows) {
			return gerr
		}

		_, _, applied, serr := s.spendBalanceThenOwedTx(ctx, q, payer, params.Invoker, cur, params.Key, params.Amount, true)
		if serr != nil {
			return serr
		}
		// The durable spend is the first transfer at these coordinates (the
		// balance debit, else the owed accrual).
		row, rerr := q.GetLedgerSpendByCoords(ctx, gen.GetLedgerSpendByCoordsParams{
			MerchantID: tenantID, CustomerID: payer.UUID(), Currency: cur,
			Operation: string(params.Key.Operation()),
			Source:    params.Key.Source(), SourceID: params.Key.SourceID(),
		})
		if rerr != nil {
			if errors.Is(rerr, pgx.ErrNoRows) {
				return fmt.Errorf("capture produced no money transfer")
			}
			return rerr
		}
		trx = moneyTransactionFromTransfer(row)
		trx.Replayed = !applied
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
// Custom units always require sufficient prepaid balance. For ISO units,
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
	userID, currency string, key IdempotencyKey, amount int64, preAuthorized bool,
) (balanceSpent, owedAccrued int64, applied bool, err error) {
	if amount <= 0 {
		return 0, 0, false, fmt.Errorf("amount must be positive")
	}
	// or#891 items 1+4: every leg posted below carries this key — the ledger
	// transfers AND the pending invoice item behind uq_invoice_items_source. A
	// blank key used to reach here and be papered over with a freshly minted
	// uuidv7, which can never collide, so every replay of a keyless spend accrued
	// a NEW invoice item. The ledger does not mint keys on a caller's behalf.
	if key.IsZero() {
		return 0, 0, false, fmt.Errorf("spend: idempotency key required")
	}
	now := s.now()
	cur := normalizeUnit(currency)
	tid, err := merchant.Require(ctx)
	if err != nil {
		return 0, 0, false, err
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()

	bal, err := s.lockBalance(ctx, q, payer, userID, cur)
	if err != nil {
		return 0, 0, false, err
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
		return 0, 0, false, lerr
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
		if IsQualifiedUnit(cur) {
			return 0, 0, false, ErrInsufficientCredits
		}
		if preAuthorized {
			// Pre-authorized capture never re-gates: the remainder becomes
			// owed/overdraft regardless of the credit line. Ensure a settings row
			// exists so the owed accrual has a home (mirrors the arrears flow).
			if err := s.ensureSettingsRowTx(ctx, q, tenantID, payerID, cur, BillingModeArrears, now); err != nil {
				return 0, 0, false, err
			}
		} else {
			// Immediate spend: the remainder can only go to owed when the account
			// has a credit line, and only up to that line.
			settingsRow, serr := q.LockMoneyAccountSettings(ctx, gen.LockMoneyAccountSettingsParams{
				MerchantID: tenantID, CustomerID: payerID, Currency: cur,
			})
			if errors.Is(serr, pgx.ErrNoRows) {
				// No settings row => prepaid default => no credit line.
				return 0, 0, false, ErrInsufficientCredits
			}
			if serr != nil {
				return 0, 0, false, serr
			}
			settings := settingsFromGen(settingsRow)
			if settings.BillingMode != BillingModeArrears {
				return 0, 0, false, ErrInsufficientCredits // prepay-only: credit limit 0
			}
			if settings.CreditLimitAmount <= 0 {
				return 0, 0, false, ErrInsufficientCredits
			}
			// or#878 ruling / or#897: exposure is LEDGER-measured. This used to sum
			// open invoices + pending invoice items, which lag the ledger by a
			// finalize cycle — so a payer could spend past the line in the window
			// between accruing and being invoiced.
			exposure, eerr := s.moneyLedger(q, tenantID).OutstandingOwed(ctx, payerID, cur)
			if eerr != nil {
				return 0, 0, false, eerr
			}
			if exposure+fromOwed > settings.CreditLimitAmount {
				return 0, 0, false, ErrInsufficientCredits // would exceed the credit line
			}
		}
	}

	if fromBalance > 0 {
		_, balanceApplied, werr := s.withdrawBalanceAndBlocks(ctx, q, payer, userID, cur, key, "", fromBalance)
		if werr != nil {
			return 0, 0, false, werr
		}
		applied = applied || balanceApplied
		balanceSpent = fromBalance
	}

	if fromOwed > 0 {
		ml := s.moneyLedger(q, tenantID)
		_, owedApplied, aerr := ml.AccrueOwedIdempotent(ctx, payerID, cur, fromOwed, key.Coord(), nil)
		if aerr != nil {
			return 0, 0, false, aerr
		}
		applied = applied || owedApplied
		if err := insertPendingInvoiceItemTx(ctx, q, tenantID, payerID, cur, txOwedAccrual, key.invoiceItemSourceID(), fromOwed, now, map[string]any{
			"operation": string(key.Operation()),
			"source":    key.Source(),
		}); err != nil {
			return 0, 0, false, err
		}
		owedAccrued = fromOwed
	}

	return balanceSpent, owedAccrued, applied, nil
}
