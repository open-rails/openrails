package credits

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
)

// Spend consumes amount from the customer's live lots, soonest expiry first:
// one credit_spend per lot drawn, each at the caller's coordinate (its
// idempotency key) and naming its lot. A lot gives what pending refunds do
// not reserve. Nothing is applied unless the lots cover amount; compose
// inside a transaction.
func (l *Lots) Spend(ctx context.Context, customer uuid.UUID, unit string, amount int64, invoker, resource string, coord ledger.Coord) (applied bool, err error) {
	if amount <= 0 {
		return false, fmt.Errorf("credits: spend amount must be positive, got %d", amount)
	}
	lots, err := l.q.ListSpendableCreditLots(ctx, gen.ListSpendableCreditLotsParams{
		MerchantID: l.merchant, CustomerID: customer, Currency: unit, AsOf: l.now(), LiveAt: l.liveAt,
	})
	if err != nil {
		return false, fmt.Errorf("credits: list lots: %w", err)
	}
	var total int64
	for _, lot := range lots {
		total += lot.Available
	}
	if total < amount {
		return false, fmt.Errorf("%w (need %d, have %d %s)", ErrInsufficientCredits, amount, total, unit)
	}
	balance, err := l.money.EnsureCustomerBalance(ctx, customer, unit)
	if err != nil {
		return false, err
	}
	revenue, err := l.money.EnsureSystemAccount(ctx, ledger.PlatformRevenue, unit)
	if err != nil {
		return false, err
	}
	left := amount
	for _, lot := range lots {
		if left == 0 {
			break
		}
		take := min(lot.Available, left)
		if take <= 0 {
			continue
		}
		id, c, inv, res := lot.ID, customer, invoker, resource
		_, lotApplied, err := l.money.ApplyIdempotent(ctx, ledger.Transfer{
			Debit: balance, Credit: revenue, Amount: take, Currency: unit, Type: ledger.CreditSpend,
			Coord: coord, CreditGrantID: &id, Customer: &c, Invoker: &inv, Resource: &res,
		})
		if err != nil {
			return applied, fmt.Errorf("credits: spend from lot %s: %w", lot.ID, err)
		}
		// One leg per lot: any leg that inserted moved money now; all legs
		// replaying means the spend was committed at this coordinate.
		applied = applied || lotApplied
		left -= take
	}
	return applied, nil
}

// LockCustomer takes the customer's spend lock (the customers row FOR
// UPDATE), so movements of its lots serialize with spends. Only effective
// inside a transaction.
func (l *Lots) LockCustomer(ctx context.Context, customer uuid.UUID) error {
	if _, err := l.q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{ID: customer, MerchantID: l.merchant}); err != nil {
		return fmt.Errorf("credits: lock customer %s: %w", customer, err)
	}
	return nil
}

// PostRunningCharges posts what a customer's running charges accrued through
// an instant, drawing the lots that had not lapsed just before it, and
// reports whether anything posted. now is the caller's clock. The money
// package, which owns running charges, installs it.
var PostRunningCharges func(ctx context.Context, q *gen.Queries, merchant, customer uuid.UUID, currency string, through, now time.Time) (posted bool, err error)

// SettleRunningCharges posts what the customer's running charges accrued
// through an instant, and its usage before it, before a lot is expired,
// revoked or refunded at it: the lot funds what accrued while it lived.
func (l *Lots) SettleRunningCharges(ctx context.Context, customer uuid.UUID, unit string, through time.Time) (bool, error) {
	posted := false
	if PostRunningCharges != nil {
		var err error
		if posted, err = PostRunningCharges(ctx, l.q, l.merchant, customer, unit, through, l.now()); err != nil {
			return posted, err
		}
	}
	accrued, err := l.settleUsage(ctx, customer, unit, &through)
	return posted || accrued, err
}

// settleUsage posts the customer's usage that occurred before liveAt (now
// when nil), drawing the lots live just before it: the usage ledger bound to
// the transaction posts it.
func (l *Lots) settleUsage(ctx context.Context, customer uuid.UUID, unit string, liveAt *time.Time) (bool, error) {
	cut := l.now()
	if liveAt != nil && liveAt.Before(cut) {
		cut = *liveAt
	}
	if usage := db.UsageLedgerOf(l.q); usage != nil {
		return usage.PostUsage(ctx, l.q, l.merchant, customer, unit, cut, l.now())
	}
	_, err := l.q.GetUsageUnposted(ctx, gen.GetUsageUnpostedParams{MerchantID: l.merchant, CustomerID: customer, Currency: unit})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return false, fmt.Errorf("credits: customer %s has usage to post first, and no usage ledger is bound", customer)
}

// ExpireLapsed moves the units left on every lot past its expiry to
// expired_credits, one transfer per lot, and returns the total. Idempotent:
// an expired lot has none left. It takes the customer's lock.
func (l *Lots) ExpireLapsed(ctx context.Context, customer uuid.UUID, unit string) (int64, error) {
	return l.ExpireLapsedBy(ctx, customer, unit, l.now())
}

// ExpireLapsedBy is ExpireLapsed for the lots that lapsed by an instant. Each
// lot first funds what the customer's running charges accrued until it
// lapsed.
func (l *Lots) ExpireLapsedBy(ctx context.Context, customer uuid.UUID, unit string, by time.Time) (int64, error) {
	if err := l.LockCustomer(ctx, customer); err != nil {
		return 0, err
	}
	lapsed, err := l.q.ListLapsedCreditLots(ctx, gen.ListLapsedCreditLotsParams{
		MerchantID: l.merchant, CustomerID: customer, Currency: unit, AsOf: by,
	})
	if err != nil {
		return 0, fmt.Errorf("credits: list lapsed lots: %w", err)
	}
	balance, err := l.money.EnsureCustomerBalance(ctx, customer, unit)
	if err != nil {
		return 0, err
	}
	expiredAccount, err := l.money.EnsureSystemAccount(ctx, ledger.ExpiredCredits, unit)
	if err != nil {
		return 0, err
	}
	var expired int64
	for _, lot := range lapsed {
		if _, err := l.SettleRunningCharges(ctx, customer, unit, lot.ExpiresAt); err != nil {
			return expired, fmt.Errorf("credits: settle running charges at lot %s: %w", lot.ID, err)
		}
		// What settled at an earlier lot's lapse may have drawn this one.
		current, err := l.Get(ctx, lot.ID)
		if err != nil {
			return expired, fmt.Errorf("credits: reload lot %s: %w", lot.ID, err)
		}
		remaining := Remaining(current)
		if remaining <= 0 {
			continue
		}
		id, c := lot.ID, customer
		if _, err := l.money.Apply(ctx, ledger.Transfer{
			Debit: balance, Credit: expiredAccount, Amount: remaining, Currency: unit, Type: ledger.CreditExpire,
			Coord:         ledger.Coord{Operation: ledger.OpCreditExpire, Source: "credit_expiry", SourceID: lot.ID.String()},
			CreditGrantID: &id, Customer: &c,
		}); err != nil {
			return expired, fmt.Errorf("credits: expire lot %s: %w", lot.ID, err)
		}
		expired += remaining
	}
	return expired, nil
}
