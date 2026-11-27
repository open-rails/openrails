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
	"github.com/open-rails/openrails/internal/modules/money/books"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
)

// Spend consumes amount from the customer's live lots, soonest expiry first:
// one credit_spend per lot drawn, each at the caller's coordinate (its
// idempotency key) and naming its lot, and its money recognised as usage
// revenue at the lot's rate. Nothing is applied unless the lots cover amount;
// compose inside a transaction.
func (l *Lots) Spend(ctx context.Context, customer uuid.UUID, unit string, amount int64, invoker, resource string, coord ledger.Coord) (applied bool, err error) {
	return l.SpendInto(ctx, customer, unit, amount, invoker, resource, coord, nil)
}

// SpendInto is Spend paying an invoice line priced in the lot's unit: the
// money goes to that line's deferred revenue (books.Line). nil is usage.
func (l *Lots) SpendInto(ctx context.Context, customer uuid.UUID, unit string, amount int64, invoker, resource string, coord ledger.Coord, into *books.Line) (applied bool, err error) {
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
	// The units book's legs of a line's spend name its invoice, in the unit.
	var invoice *uuid.UUID
	if into != nil && into.Invoice != uuid.Nil {
		invoice = &into.Invoice
	}
	balance, err := l.money.EnsureCustomerBalance(ctx, customer, unit)
	if err != nil {
		return false, err
	}
	revenue, err := l.money.EnsureSystemAccount(ctx, ledger.UnitsRedeemed, unit)
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
		leg, lotApplied, err := l.money.ApplyIdempotent(ctx, ledger.Transfer{
			Debit: balance, Credit: revenue, Amount: take, Currency: unit, Type: ledger.CreditSpend,
			Coord: coord, CreditGrantID: &id, Customer: &c, Invoker: &inv, Resource: &res, Invoice: invoice,
		})
		if err != nil {
			return applied, fmt.Errorf("credits: spend from lot %s: %w", lot.ID, err)
		}
		if err := l.move(ctx, lot.ID, books.CreditSpent, leg, into); err != nil {
			return applied, err
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

// SettleRunningCharges posts the customer's usage before an instant and what
// its running charges accrued through it, before a lot is expired, revoked
// or refunded at it: the lot funds what accrued while it lived. An instant
// not before now posts all the usage, after expiring the lots that lapsed by
// now, each once what came before its lapse posted.
func (l *Lots) SettleRunningCharges(ctx context.Context, customer uuid.UUID, unit string, through time.Time) (bool, error) {
	if !through.Before(l.now()) {
		if _, err := l.ExpireLapsedBy(ctx, customer, unit, l.now()); err != nil {
			return false, err
		}
	}
	return l.settleUsage(ctx, customer, unit, through, through)
}

// settleUsage posts the customer's usage that occurred before cut and its
// running charges through through (none when zero), drawing the lots live
// just before cut; a cut not before now posts all the usage. The usage
// ledger bound to the transaction posts them in one receipt.
func (l *Lots) settleUsage(ctx context.Context, customer uuid.UUID, unit string, cut, through time.Time) (bool, error) {
	if !cut.Before(l.now()) {
		cut = time.Time{}
	}
	if usage := db.UsageLedgerOf(l.q); usage != nil {
		return usage.PostUsage(ctx, l.q, l.merchant, customer, unit, cut, through, l.now(), "credit")
	}
	_, err := l.q.GetUsagePending(ctx, gen.GetUsagePendingParams{MerchantID: l.merchant, CustomerID: customer, Currency: unit})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return false, fmt.Errorf("credits: customer %s has usage or running charges to post first, and no usage ledger is bound", customer)
}

// ExpireLapsed moves the units left on every lot past its expiry to
// units_expired, one transfer per lot, and their money to breakage, and
// returns the total. Idempotent: an expired lot has none left. It takes the
// customer's lock.
func (l *Lots) ExpireLapsed(ctx context.Context, customer uuid.UUID, unit string) (int64, error) {
	return l.ExpireLapsedBy(ctx, customer, unit, l.now())
}

// ExpireLapsedBy is ExpireLapsed for the lots that lapsed by an instant. Each
// lot first funds the customer's usage and what its running charges accrued
// until it lapsed; then the usage since posts from the lots left.
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
	expiredAccount, err := l.money.EnsureSystemAccount(ctx, ledger.UnitsExpired, unit)
	if err != nil {
		return 0, err
	}
	var expired int64
	for _, lot := range lapsed {
		if _, err := l.settleUsage(ctx, customer, unit, lot.ExpiresAt, lot.ExpiresAt); err != nil {
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
		leg, err := l.money.Apply(ctx, ledger.Transfer{
			Debit: balance, Credit: expiredAccount, Amount: remaining, Currency: unit, Type: ledger.CreditExpire,
			Coord:         ledger.Coord{Operation: ledger.OpCreditExpire, Source: "credit_expiry", SourceID: lot.ID.String()},
			CreditGrantID: &id, Customer: &c,
		})
		if err != nil {
			return expired, fmt.Errorf("credits: expire lot %s: %w", lot.ID, err)
		}
		if err := l.move(ctx, lot.ID, books.CreditExpired, leg, nil); err != nil {
			return expired, err
		}
		expired += remaining
	}
	if len(lapsed) > 0 {
		if _, err := l.settleUsage(ctx, customer, unit, time.Time{}, time.Time{}); err != nil {
			return expired, fmt.Errorf("credits: post usage after the expiries: %w", err)
		}
	}
	return expired, nil
}
