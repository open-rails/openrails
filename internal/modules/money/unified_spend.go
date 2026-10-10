package money

import (
	"context"
	"errors"
	"fmt"

	"github.com/open-rails/openrails/internal/shared/moneyutil"

	"github.com/jackc/pgx/v5"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
)

// Unified credit-line model. An account spends prepaid balance first, then
// creates pending invoice items up to its admin-set credit_limit_amount.
// Prepay-only = credit limit 0; arrears = explicit credit line.

// SpendParams is a unified immediate spend (balance first, then owed). Key is
// built with money.NewIdempotencyKey.
type SpendParams struct {
	Payer    *identity.CustomerID
	Invoker  string
	Currency string
	Amount   int64
	Key      IdempotencyKey
}

// SpendCredits debits an account balance-first-then-owed in one transaction,
// gated by the credit line. Idempotent on (merchant, payer, currency,
// operation=spend, source, source_id): the key is required, and a replay with
// a different Amount gets ErrIdempotencyKeyReused. Returns
// ErrInsufficientCredits when balance + remaining credit line cannot cover the
// amount. Replayed is true when the coordinate was already committed and
// nothing moved.
func (s *MoneyService) SpendCredits(ctx context.Context, params SpendParams) (*models.MoneyTransaction, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if params.Amount <= 0 {
		return nil, fmt.Errorf("amount must be positive")
	}
	if err := params.Key.RequireOperation(OpSpend); err != nil {
		return nil, err
	}
	cur := normalizeCurrency(params.Currency)
	if err := moneyutil.ValidateCurrency(cur); err != nil {
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
			// The pre-check is a fast path and body guard; the unique index under
			// spendBalanceThenOwedTx enforces once-only, so `applied` stays
			// authoritative when two transactions race past it.
			//
			// A replay must not reach the spend arithmetic: the balance already
			// moved, so re-deriving it would split the charge and could deny a
			// prepaid payer, answering a replay with a hard failure.
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

// CaptureAuthorized records the durable money movement for an admitted
// request, idempotent on (merchant, payer, currency, operation=capture,
// source, source_id).
//
// Capture records reality unconditionally: admission is the only gate, so it
// never re-gates the credit line. It draws the prepaid balance first and
// records any remainder as owed/overdraft, even for a prepaid payer with no
// line. Re-gating would make a served request fail on every retry.
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
	cur := normalizeCurrency(params.Currency)
	if err := moneyutil.ValidateCurrency(cur); err != nil {
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
		// Compare the total already posted at this coordinate (not the first
		// transfer, which is one FIFO lot's take) and refuse a changed body.
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

// spendBalanceThenOwedTx debits amount within an existing tx: prepaid balance
// first (FIFO credit lots → ledger spend transfers), any remainder accrued to
// pending invoice items + an arrears-liability transfer. The caller has locked
// the balance row and handled idempotency. Returns the amounts drawn from
// balance and accrued to owed (either may be 0).
//
// preAuthorized selects the gating contract:
//   - false (SpendCredits): the remainder is gated by the credit line; a payer
//     without one, or a spend past it, gets ErrInsufficientCredits and nothing
//     is debited.
//   - true (capture of an admitted request): never re-gates; the remainder is
//     recorded as owed/overdraft even without a credit line. Admission was the
//     gate; capture records reality.
func (s *MoneyService) spendBalanceThenOwedTx(
	ctx context.Context, q *gen.Queries, payer identity.CustomerID,
	userID, currency string, key IdempotencyKey, amount int64, preAuthorized bool,
) (balanceSpent, owedAccrued int64, applied bool, err error) {
	if amount <= 0 {
		return 0, 0, false, fmt.Errorf("amount must be positive")
	}
	// Every leg below carries this key: the ledger transfers and the pending
	// invoice item. The ledger never mints a key for a caller: a fresh one
	// would make every replay accrue a new item.
	if key.IsZero() {
		return 0, 0, false, fmt.Errorf("spend: idempotency key required")
	}
	now := s.now()
	cur := normalizeCurrency(currency)
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
	// remainders that CreditSpend cannot draw: cap the balance leg at the
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
		if preAuthorized {
			// Pre-authorized capture never re-gates: the remainder becomes
			// owed/overdraft regardless of the credit line. An involuntary
			// overdraft leaves a prepaid customer prepaid: the owed blocks new
			// holds until funding repays it.
			if err := s.ensureSettingsRowTx(ctx, q, tenantID, payerID, cur, BillingModePrepaid, now); err != nil {
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
			// Exposure is ledger-measured; invoices lag it by a finalize cycle.
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
