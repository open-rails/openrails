package grants

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
)

// ErrInsufficientCredits is returned when a spend exceeds the customer's
// spendable credit-lot remainder.
var ErrInsufficientCredits = errors.New("grants: insufficient credits")

// CreditSpend consumes `amount` of credits FIFO across the customer's live
// credit lots (soonest-expiring first), emitting one #512 ledger spend transfer
// per lot drawn. Each transfer carries the OPERATION coordinate (operation,
// source, sourceID) — the caller's idempotency key, discriminated by the kind
// of money write (or#894) — AND grant_id = the lot it draws from. Per-lot remaining is derived from the ledger — no
// mutable remaining column. Atomic: nothing is applied unless the full amount is
// covered. Compose inside a tx for isolation.
func (l *Ledger) CreditSpend(ctx context.Context, customer uuid.UUID, currency string, amount int64, invoker, resource string, coord ledger.Coord) (applied bool, err error) {
	if amount <= 0 {
		return false, fmt.Errorf("grants: spend amount must be positive, got %d", amount)
	}
	lots, err := l.q.ListSpendableCreditLots(ctx, gen.ListSpendableCreditLotsParams{
		MerchantID: l.merchant, CustomerID: customer, Currency: currency, AsOf: l.now(),
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

// LockCustomer takes the per-customer spend mutex (#491/#677): the same
// customers-row FOR UPDATE money's lockBalance takes, so lot-consuming/
// reversing writes serialize with spends. Only effective inside a tx.
func (l *Ledger) LockCustomer(ctx context.Context, customer uuid.UUID) error {
	if _, err := l.q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{
		ID: customer, MerchantID: l.merchant,
	}); err != nil {
		return fmt.Errorf("grants: lock customer %s: %w", customer, err)
	}
	return nil
}

// ExpireLapsed claws the unspent remainder of every past-expiry credit lot to
// the expired_credits account (one #512 transfer per lot). Idempotent: an
// already-expired lot has zero remainder and is skipped. Returns total expired.
// Compose inside a tx: it takes the per-customer spend lock (#677) so a
// concurrent spend can't draw a lot between the remaining read and the
// credit_expire write.
func (l *Ledger) ExpireLapsed(ctx context.Context, customer uuid.UUID, currency string) (int64, error) {
	if err := l.LockCustomer(ctx, customer); err != nil {
		return 0, err
	}
	lots, err := l.q.ListLapsedCreditLots(ctx, gen.ListLapsedCreditLotsParams{
		MerchantID: l.merchant, CustomerID: customer, Currency: currency, AsOf: l.now(),
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
		lotID, c := lot.ID, customer
		if _, err := l.money.Apply(ctx, ledger.Transfer{
			Debit: cust, Credit: exp, Amount: lot.Remaining, Currency: currency, Type: ledger.CreditExpire,
			Coord:   ledger.Coord{Operation: ledger.OpCreditExpire, Source: "credit_expiry", SourceID: lot.ID.String()},
			GrantID: &lotID, Customer: &c,
		}); err != nil {
			return expired, fmt.Errorf("grants: expire lot %s: %w", lot.ID, err)
		}
		expired += lot.Remaining
	}
	return expired, nil
}
