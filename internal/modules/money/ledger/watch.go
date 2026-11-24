package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/balancelook"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/limits"
)

// WatchChange is told, in its transaction, of a change to what a customer may
// spend in a currency. It queues a look at the balance, in that transaction,
// when the last look said each change needs one (a running charge or a lot
// that lapses moves the run-out), when the balance is at or under a line
// whose notice is armed, or when a funding lifts it over one whose notice was
// sent. Any other change queues nothing. A watch it queues is also marked due,
// so the billing pass looks at it if the look fails. Every caller holds the
// customer's LockCustomerForSpend, which the look takes too (WatchAvailable's
// hold the watch instead). Only the units
// book reaches it: a general-ledger posting changes no spendable balance.
func WatchChange(ctx context.Context, q *gen.Queries, merchant, customer uuid.UUID, currency string, funded bool, now time.Time) error {
	key := gen.GetBalanceWatchParams{MerchantID: merchant, CustomerID: customer, Currency: currency}
	w, err := q.GetBalanceWatch(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if funded && w.FundedAt == nil {
		if err := q.StampBalanceWatchFunded(ctx, gen.StampBalanceWatchFundedParams{MerchantID: merchant, CustomerID: customer, Currency: currency, Now: now}); err != nil {
			return err
		}
	}
	look := w.LookEachChange
	if !look {
		available, err := availableNow(ctx, q, merchant, customer, currency, now)
		if err != nil {
			return err
		}
		look = crossed(w, available, funded)
	}
	if !look {
		return nil
	}
	return markDue(ctx, q, merchant, customer, currency, now)
}

// WatchAvailable is WatchChange for usage, given the watch its caller holds
// against a look (ShareBalanceWatches) and the available amount it read
// after taking it.
func WatchAvailable(ctx context.Context, q *gen.Queries, w gen.BillingBalanceWatch, available int64, now time.Time) error {
	if !w.LookEachChange && !crossed(w, available, false) {
		return nil
	}
	return markDue(ctx, q, w.MerchantID, w.CustomerID, w.Currency, now)
}

// markDue marks the watch due and queues its look. A watch already due has
// its look queued, or failed for the billing pass: the look reads this
// change after it.
func markDue(ctx context.Context, q *gen.Queries, merchant, customer uuid.UUID, currency string, now time.Time) error {
	marked, err := q.MarkBalanceWatchDue(ctx, gen.MarkBalanceWatchDueParams{MerchantID: merchant, CustomerID: customer, Currency: currency, Now: now})
	if err != nil || marked == 0 {
		return err
	}
	return QueueLook(ctx, q, merchant, customer, currency, now)
}

// QueueLook queues a look at a balance its caller marked due at now, in q's
// transaction. Without a River producer the billing pass looks at the mark.
func QueueLook(ctx context.Context, q *gen.Queries, merchant, customer uuid.UUID, currency string, now time.Time) error {
	at := now.UTC()
	_, err := db.InsertRiverJobWith(ctx, q, balancelook.Args{MerchantID: merchant, CustomerID: customer, Currency: currency, At: &at}, nil)
	if errors.Is(err, db.ErrNoRiverProducer) {
		return nil
	}
	return err
}

// crossed: the balance is at or under a line whose notice is armed, or a
// funding lifted it over one whose notice was sent.
func crossed(w gen.BillingBalanceWatch, available int64, funded bool) bool {
	at := func(line *int64) bool { return line != nil && available <= *line }
	if w.LowNotifiedAt == nil && at(w.LowLineAmount) || w.DepletedAt == nil && at(w.FloorAmount) {
		return true
	}
	return funded && (w.LowNotifiedAt != nil && !at(w.LowLineAmount) || w.DepletedAt != nil && !at(w.FloorAmount))
}

// availableNow is the balance less what is held, as a look reads it.
func availableNow(ctx context.Context, q *gen.Queries, merchant, customer uuid.UUID, currency string, now time.Time) (int64, error) {
	l := New(q, merchant)
	account, found, err := l.CustomerBalanceAccountID(ctx, customer, currency)
	if err != nil || !found {
		return 0, err
	}
	balance, err := l.Balance(ctx, account)
	if err != nil {
		return 0, err
	}
	held, err := q.GetFinancialHeldAmount(ctx, gen.GetFinancialHeldAmountParams{
		MerchantID: merchant, CustomerID: customer, Currency: currency, AsOf: now, HeldSince: limits.HeldSince(now), LeaseLiveSince: limits.LeaseLiveSince(now),
	})
	if err != nil {
		return 0, fmt.Errorf("ledger: held amount: %w", err)
	}
	unposted, err := UnpostedUsage(ctx, q, merchant, customer, currency)
	return balance - held.Held - unposted, err
}

// UnpostedUsage is what a customer's usage in a currency costs beyond what
// posted, through the usage ledger bound to q's transaction. Without one, a
// customer with unposted usage is an error.
func UnpostedUsage(ctx context.Context, q *gen.Queries, merchant, customer uuid.UUID, currency string) (int64, error) {
	if usage := db.UsageLedgerOf(q); usage != nil {
		return usage.UnpostedUsage(ctx, q, merchant, customer, currency)
	}
	_, err := q.GetUsageUnposted(ctx, gen.GetUsageUnpostedParams{MerchantID: merchant, CustomerID: customer, Currency: currency})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("ledger: customer %s has unposted usage, and no usage ledger is bound", customer)
}

// WatchCustomerBalance marks the customer's watch due after a change to its
// billing mode, credit line or line, and queues the look.
func WatchCustomerBalance(ctx context.Context, q *gen.Queries, arg gen.WatchCustomerBalanceParams) error {
	n, err := q.WatchCustomerBalance(ctx, arg)
	if err != nil || n == 0 {
		return err
	}
	return QueueLook(ctx, q, arg.MerchantID, arg.CustomerID, arg.Currency, arg.Now)
}

// WatchLapsingCredit watches the balance of a lot that lapses, and queues the
// look that schedules the lapse.
func WatchLapsingCredit(ctx context.Context, q *gen.Queries, arg gen.WatchLapsingCreditParams) error {
	n, err := q.WatchLapsingCredit(ctx, arg)
	if err != nil || n == 0 {
		return err
	}
	return QueueLook(ctx, q, arg.MerchantID, arg.CustomerID, arg.Currency, arg.Now)
}
