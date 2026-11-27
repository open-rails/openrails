package money

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money/books"
)

// Unified credit-line model. An account spends prepaid balance first, then
// creates pending owed items up to its admin-set credit_limit_amount.
// Prepay-only = credit limit 0; arrears = explicit credit line.

// spendBalanceThenOwedTx debits amount within an existing tx: prepaid balance
// first (FIFO credit lots → ledger spend transfers), any remainder accrued to
// pending owed items + an arrears-liability transfer. The caller has locked
// the balance row and handled idempotency. Returns the amounts drawn from
// balance and accrued to owed (either may be 0).
//
// preAuthorized selects the gating contract:
//   - false: the remainder is gated by the credit line; a customer
//     without one, or a spend past it, gets ErrInsufficientCredits and nothing
//     is debited.
//   - true (capture of an admitted request): never re-gates; the remainder is
//     recorded as owed/overdraft even without a credit line. Admission was the
//     gate; capture records reality.
func (s *MoneyService) spendBalanceThenOwedTx(
	ctx context.Context, q *gen.Queries, customer identity.CustomerID,
	userID, currency string, key IdempotencyKey, amount int64, preAuthorized bool,
) (balanceSpent, owedAccrued int64, applied bool, err error) {
	if amount <= 0 {
		return 0, 0, false, fmt.Errorf("amount must be positive")
	}
	// Every leg below carries this key: the ledger transfers and the pending
	// owed item. The ledger never mints a key for a caller: a fresh one
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
	customerID := customer.UUID()

	bal, err := s.lockBalance(ctx, q, customer, userID, cur)
	if err != nil {
		return 0, 0, false, err
	}
	available := max(s.spendable(bal), 0)
	// The ledger balance holds a lapsed lot until its expiry posts, and a
	// spend never draws it: cap the balance leg at the live lots.
	lots, lerr := q.ListSpendableCreditLots(ctx, gen.ListSpendableCreditLotsParams{
		MerchantID: tenantID, CustomerID: customerID, Currency: cur, AsOf: now, LiveAt: s.lotsLiveAt(),
	})
	if lerr != nil {
		return 0, 0, false, lerr
	}
	var spendable int64
	for _, lot := range lots {
		spendable += lot.Available
	}
	// Admitted usage in a merchant's unit takes what the lots hold, whatever
	// else is pending: it is never owed.
	if available > spendable || preAuthorized && IsMerchantUnit(cur) {
		available = spendable
	}
	fromBalance := amount
	if fromBalance > available {
		fromBalance = available
	}
	fromOwed := amount - fromBalance

	// A merchant's units are prepaid, never owed: a caller forgives what the
	// lots cannot take before it charges (spendableUnits).
	if fromOwed > 0 && IsMerchantUnit(cur) {
		return 0, 0, false, ErrInsufficientCredits
	}

	if fromOwed > 0 {
		if preAuthorized {
			// Pre-authorized capture never re-gates: the remainder becomes
			// owed/overdraft regardless of the credit line. An involuntary
			// overdraft leaves a prepaid customer prepaid: the owed blocks new
			// holds until funding repays it.
			if err := s.ensureSettingsRowTx(ctx, q, tenantID, customerID, cur, BillingModePrepaid, now); err != nil {
				return 0, 0, false, err
			}
		} else {
			// Immediate spend: the remainder can only go to owed when the account
			// has a credit line, and only up to that line.
			settingsRow, serr := q.LockMoneyAccountSettings(ctx, gen.LockMoneyAccountSettingsParams{
				MerchantID: tenantID, CustomerID: customerID, Currency: cur,
			})
			if errors.Is(serr, pgx.ErrNoRows) {
				// No settings row => prepaid default => no credit line.
				return 0, 0, false, ErrInsufficientCredits
			}
			if serr != nil {
				return 0, 0, false, serr
			}
			capacity, err := s.usageAdmissionCapacity(ctx, q, customer, cur, AdmissionCapacity{BillingMode: settingsRow.BillingMode, CreditLimit: settingsRow.CreditLimitAmount})
			if err != nil {
				return 0, 0, false, err
			}
			if capacity.BillingMode != BillingModeArrears || capacity.CreditLimit <= 0 {
				return 0, 0, false, ErrInsufficientCredits
			}
			exposure, err := s.moneyLedger(q, tenantID).OutstandingOwed(ctx, customerID, cur)
			if err != nil {
				return 0, 0, false, err
			}
			if fromOwed > capacity.CreditLimit-max(exposure, 0) {
				return 0, 0, false, ErrInsufficientCredits
			}

		}
	}

	if fromBalance > 0 {
		_, balanceApplied, werr := s.withdrawBalanceAndBlocks(ctx, q, customer, userID, cur, key, fromBalance)
		if werr != nil {
			return 0, 0, false, werr
		}
		applied = applied || balanceApplied
		balanceSpent = fromBalance
	}

	if fromOwed > 0 {
		ml := s.moneyLedger(q, tenantID)
		_, owedApplied, aerr := ml.AccrueOwedIdempotent(ctx, customerID, cur, fromOwed, key.Coord(), nil)
		if aerr != nil {
			return 0, 0, false, aerr
		}
		applied = applied || owedApplied
		parts := s.owedParts(key, fromOwed, now)
		// What it leaves owed is usage revenue receivable, dated as its owed
		// items are: one accrual per date, the first at the owed accrual's
		// coordinate.
		if owedApplied {
			for i, a := range accrualsByDate(parts) {
				coord := key.Coord()
				if i > 0 {
					coord.SourceID += "@" + a.at.UTC().Format(instantLayout)
				}
				if err := books.New(q, tenantID).AccrueUsage(ctx, customerID, cur, a.amount, coord, a.at, nil); err != nil {
					return 0, 0, false, err
				}
			}
		}
		for _, p := range parts {
			if err := insertPendingOwedItemTx(ctx, q, tenantID, customerID, cur, txOwedAccrual, p.sourceID, p.amount, p.at, p.metadata); err != nil {
				return 0, 0, false, err
			}
		}
		if err := s.queueUsageBilling(ctx, q, customerID, cur); err != nil {
			return 0, 0, false, err
		}
		owedAccrued = fromOwed
	}

	return balanceSpent, owedAccrued, applied, nil
}

// owedPart is one owed item a spend leaves for its usage invoice.
type owedPart struct {
	sourceID string
	amount   int64
	at       time.Time
	metadata map[string]any
}

// owedParts split what a spend left owed into its owed items: one dated now,
// or one per share of a posting (a period's meter, its running charges),
// each dated and starting as its share says, so the invoice bills each on
// its own line.
func (s *MoneyService) owedParts(key IdempotencyKey, owed int64, now time.Time) []owedPart {
	if s.posting == nil || len(s.posting.owed) == 0 {
		return []owedPart{{sourceID: key.owedItemSourceID(), amount: owed, at: now,
			metadata: map[string]any{"operation": string(key.Operation()), "source": key.Source()}}}
	}
	weights := make([]int64, len(s.posting.owed))
	for i, o := range s.posting.owed {
		weights[i] = o.weight
	}
	var out []owedPart
	for i, amount := range share(owed, weights) {
		o := s.posting.owed[i]
		from := o.from.UTC().Format(time.RFC3339Nano)
		out = append(out, owedPart{sourceID: key.owedItemSourceID() + "/" + o.source + "@" + from, amount: amount, at: o.at,
			metadata: map[string]any{"operation": string(key.Operation()), "source": o.source, owedPeriodStartKey: from}})
	}
	return out
}

// accrualsByDate sums owed parts by date, earliest first.
func accrualsByDate(parts []owedPart) []owedPart {
	var out []owedPart
	for _, p := range parts {
		if p.amount <= 0 {
			continue
		}
		i := slices.IndexFunc(out, func(a owedPart) bool { return a.at.Equal(p.at) })
		if i < 0 {
			out = append(out, owedPart{at: p.at})
			i = len(out) - 1
		}
		out[i].amount = addSat(out[i].amount, p.amount)
	}
	slices.SortFunc(out, func(a, b owedPart) int { return a.at.Compare(b.at) })
	return out
}

// spendableUnits is what a customer's lots in a merchant's unit can take now,
// those live just before a posting's cut: what a charge in the unit forgives
// beyond it is never owed.
func (s *MoneyService) spendableUnits(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID, unit string) (int64, error) {
	lots, err := q.ListSpendableCreditLots(ctx, gen.ListSpendableCreditLotsParams{
		MerchantID: merchantID, CustomerID: customer, Currency: unit, AsOf: s.now(), LiveAt: s.lotsLiveAt(),
	})
	if err != nil {
		return 0, err
	}
	var spendable int64
	for _, lot := range lots {
		spendable = addSat(spendable, lot.Available)
	}
	return spendable, nil
}
