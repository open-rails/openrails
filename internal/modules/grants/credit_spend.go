package grants

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
)

// ErrInsufficientCredits is returned when a spend exceeds the customer's
// spendable credit-lot remainder.
var ErrInsufficientCredits = errors.New("grants: insufficient credits")

// CreditSpend consumes amount FIFO across the customer's live credit lots
// (soonest-expiring first), one ledger spend transfer per lot drawn, each
// carrying the caller's coordinate (its idempotency key) and the lot's
// grant_id. Lot remainders derive from the ledger. Nothing is applied unless
// the full amount is covered; compose inside a tx.
func (l *Ledger) CreditSpend(ctx context.Context, customer uuid.UUID, currency string, amount int64, invoker, resource string, coord ledger.Coord) (applied bool, err error) {
	if amount <= 0 {
		return false, fmt.Errorf("grants: spend amount must be positive, got %d", amount)
	}
	lots, err := l.q.ListSpendableCreditLots(ctx, gen.ListSpendableCreditLotsParams{
		MerchantID: l.merchant, CustomerID: customer, Currency: currency, AsOf: l.now(), LiveAt: l.liveAt,
	})
	if err != nil {
		return false, fmt.Errorf("grants: list credit lots: %w", err)
	}
	var total int64
	for _, lot := range lots {
		if lot.Remaining > 0 {
			total += lot.Remaining
		}
	}
	if total < amount {
		return false, fmt.Errorf("%w (need %d, have %d %s)", ErrInsufficientCredits, amount, total, currency)
	}

	cust, err := l.money.EnsureCustomerBalance(ctx, customer, currency)
	if err != nil {
		return false, err
	}
	rev, err := l.money.EnsureSystemAccount(ctx, ledger.PlatformRevenue, currency)
	if err != nil {
		return false, err
	}

	left := amount
	for _, lot := range lots {
		if left == 0 {
			break
		}
		if lot.Remaining <= 0 {
			continue
		}
		take := lot.Remaining
		if take > left {
			take = left
		}
		lotID, c, inv, res := lot.ID, customer, invoker, resource
		_, lotApplied, aerr := l.money.ApplyIdempotent(ctx, ledger.Transfer{
			Debit: cust, Credit: rev, Amount: take, Currency: currency, Type: ledger.CreditSpend,
			Coord: coord, GrantID: &lotID, Customer: &c, Invoker: &inv, Resource: &res,
		})
		if aerr != nil {
			return applied, fmt.Errorf("grants: credit spend from lot %s: %w", lot.ID, aerr)
		}
		// A spend fans out one leg per lot. Any leg that actually inserted means
		// this operation moved money now; all legs replaying means the whole
		// spend was already committed at this coordinate.
		applied = applied || lotApplied
		left -= take
	}
	return applied, nil
}

// LockCustomer takes the per-customer spend lock (the customers row FOR UPDATE
// money's lockBalance takes), so lot-consuming/reversing writes serialize with
// spends. Only effective inside a tx.
func (l *Ledger) LockCustomer(ctx context.Context, customer uuid.UUID) error {
	if _, err := l.q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{
		ID: customer, MerchantID: l.merchant,
	}); err != nil {
		return fmt.Errorf("grants: lock customer %s: %w", customer, err)
	}
	return nil
}

// PostRunningCharges posts what a customer's running charges accrued through
// an instant to the ledger, drawing the credit lots that had not lapsed just
// before it, and reports whether anything was posted. now is the caller's
// clock. The money package, which owns running charges, installs it.
var PostRunningCharges func(ctx context.Context, q *gen.Queries, merchant, customer uuid.UUID, currency string, through, now time.Time) (posted bool, err error)

// SettleRunningCharges posts what the customer's running charges accrued
// through an instant, before a credit lot is expired, revoked or refunded at
// it: the lot funds what accrued while it lived.
func (l *Ledger) SettleRunningCharges(ctx context.Context, customer uuid.UUID, currency string, through time.Time) (bool, error) {
	if PostRunningCharges == nil {
		return false, nil
	}
	return PostRunningCharges(ctx, l.q, l.merchant, customer, currency, through, l.now())
}

// DrawLotsLiveAt makes CreditSpend draw the lots that had not lapsed by t in
// place of now: a running charge's posting through the instant after t. nil
// restores now.
func (l *Ledger) DrawLotsLiveAt(t *time.Time) { l.liveAt = t }

// ExpireLapsed moves the unspent remainder of every past-expiry credit lot to
// expired_credits, one transfer per lot, and returns the total. Idempotent: an
// expired lot has zero remainder. Compose inside a tx: it takes the spend lock
// so a concurrent spend can't draw a lot between the read and the write.
func (l *Ledger) ExpireLapsed(ctx context.Context, customer uuid.UUID, currency string) (int64, error) {
	return l.ExpireLapsedBy(ctx, customer, currency, l.now())
}

// ExpireLapsedBy is ExpireLapsed for the lots that lapsed by an instant. Each
// lot first funds what the customer's running charges accrued until it lapsed.
func (l *Ledger) ExpireLapsedBy(ctx context.Context, customer uuid.UUID, currency string, by time.Time) (int64, error) {
	if err := l.LockCustomer(ctx, customer); err != nil {
		return 0, err
	}
	lots, err := l.q.ListLapsedCreditLots(ctx, gen.ListLapsedCreditLotsParams{
		MerchantID: l.merchant, CustomerID: customer, Currency: currency, AsOf: by,
	})
	if err != nil {
		return 0, fmt.Errorf("grants: list lapsed lots: %w", err)
	}
	cust, err := l.money.EnsureCustomerBalance(ctx, customer, currency)
	if err != nil {
		return 0, err
	}
	exp, err := l.money.EnsureSystemAccount(ctx, ledger.ExpiredCredits, currency)
	if err != nil {
		return 0, err
	}
	var expired int64
	for _, lot := range lots {
		if lot.Remaining <= 0 {
			continue
		}
		remaining := lot.Remaining
		posted, err := l.SettleRunningCharges(ctx, customer, currency, lot.EndsAt)
		if err != nil {
			return expired, fmt.Errorf("grants: settle running charges at lot %s: %w", lot.ID, err)
		}
		if posted {
			if remaining, err = l.q.GetCreditLotRemaining(ctx, gen.GetCreditLotRemainingParams{MerchantID: l.merchant, GrantID: lot.ID}); err != nil {
				return expired, fmt.Errorf("grants: lot remaining for %s: %w", lot.ID, err)
			}
			if remaining <= 0 {
				continue
			}
		}
		lotID, c := lot.ID, customer
		if _, err := l.money.Apply(ctx, ledger.Transfer{
			Debit: cust, Credit: exp, Amount: remaining, Currency: currency, Type: ledger.CreditExpire,
			Coord:   ledger.Coord{Operation: ledger.OpCreditExpire, Source: "credit_expiry", SourceID: lot.ID.String()},
			GrantID: &lotID, Customer: &c,
		}); err != nil {
			return expired, fmt.Errorf("grants: expire lot %s: %w", lot.ID, err)
		}
		expired += remaining
	}
	return expired, nil
}
