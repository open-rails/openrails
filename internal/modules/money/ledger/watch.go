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
	"github.com/open-rails/openrails/internal/retention"
)

// WatchChange is told, in its transaction, of a change to what a customer may
// spend in a currency. It queues a look at the balance, in that transaction,
// when the last look said each change needs one (a running charge or a lot
// that lapses moves the run-out), when the balance is at or under a line
// whose notice is armed, or when a funding lifts it over one whose notice was
// sent. Any other change queues nothing. A watch it queues is also marked due,
// so the billing pass looks at it if the look fails. Every caller holds the
// customer's LockCustomerForSpend, which the look takes too.
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
	// A watch already due has its look queued, or failed for the billing
	// pass: the look reads this change under the customer's lock.
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
		MerchantID: merchant, CustomerID: customer, Currency: currency, AsOf: now, HeldSince: now.Add(-retention.AdmissionMaxHold),
	})
	if err != nil {
		return 0, fmt.Errorf("ledger: held amount: %w", err)
	}
	return balance - held.Held, nil
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
