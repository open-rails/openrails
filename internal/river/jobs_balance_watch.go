package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/balancelook"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/modules/money"
)

const (
	KindBalanceWatch = "openrails.balance_watch"

	// balanceWatchBatch is how many due watches one read takes. The read is
	// indexed on the due instant, so a pass costs what is due, never what is
	// watched.
	balanceWatchBatch = 200
)

// BalanceWatchWorker looks at every customer whose balance watch is due: it
// expires the credit lots that lapsed, posts the daily checkpoint of its
// running charges, tells the host and the customer of a low or depleted
// balance, and writes when to look next (money.WatchBalanceInTx), on the money
// service's clock. An idle pass reads no row. A failed look stays due and
// fails the pass once the others are done.
type BalanceWatchWorker struct {
	DB    *db.DB
	Money *money.MoneyService
	Log   *slog.Logger
}

// DueBy reports a watch due since before at: a lapse, a running charge's
// checkpoint or a crossing nothing looked at.
func (w BalanceWatchWorker) DueBy(ctx context.Context, at time.Time) (bool, error) {
	return w.DB.GenDirectory().BalanceWatchDueBefore(ctx, at)
}

func (w BalanceWatchWorker) Pass(ctx context.Context) error {
	now := w.Money.Clock().Now().UTC()
	logger := w.Log.With("worker", KindBalanceWatch)
	args := gen.ListDueBalanceWatchesParams{Now: now, RowLimit: balanceWatchBatch}
	var failed error
	for {
		due, err := w.DB.GenDirectory().ListDueBalanceWatches(ctx, args)
		if err != nil {
			return errors.Join(failed, err)
		}
		for _, row := range due {
			err := w.DB.RunInMerchantScope(ctx, billing.MerchantID(row.MerchantID), "balance watch", func(ctx context.Context) error {
				return w.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
					return w.Money.WatchBalanceInTx(ctx, w.DB.NewWithPgxTx(tx), identity.CustomerID(row.CustomerID), row.Currency)
				})
			})
			if err != nil {
				// One customer's failure must not hold up the rest: its watch stays due.
				logger.ErrorContext(ctx, "balance watch: look failed; continuing", logx.Err(err), "merchant_id", row.MerchantID, "customer_id", row.CustomerID, "currency", row.Currency)
				failed = errors.Join(failed, fmt.Errorf("balance watch: merchant %s customer %s %s: %w", row.MerchantID, row.CustomerID, row.Currency, err))
			}
		}
		if len(due) < balanceWatchBatch {
			return failed
		}
		last := due[len(due)-1]
		args.AfterAt, args.AfterMerchantID, args.AfterCustomerID, args.AfterCurrency = &last.NextCheckAt, &last.MerchantID, &last.CustomerID, &last.Currency
	}
}

// BalanceLookWorker looks at one balance whose watch is due: a change crossed
// one of its lines, or the instant its last look named came. A look for an
// instant a later look moved is left alone.
type BalanceLookWorker struct {
	WorkerDefaults[balancelook.Args]
	DB    *db.DB
	Money *money.MoneyService
}

func (BalanceLookWorker) Kind() string { return balancelook.Kind }

func (w BalanceLookWorker) Work(ctx context.Context, job *river.Job[balancelook.Args]) error {
	a := job.Args
	now := w.Money.Clock().Now().UTC()
	return w.DB.RunInMerchantScope(ctx, billing.MerchantID(a.MerchantID), "balance look", func(ctx context.Context) error {
		return w.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			d := w.DB.NewWithPgxTx(tx)
			watch, err := d.Gen(ctx).GetBalanceWatch(ctx, gen.GetBalanceWatchParams{MerchantID: a.MerchantID, CustomerID: a.CustomerID, Currency: a.Currency})
			if errors.Is(err, pgx.ErrNoRows) || err == nil && (watch.NextCheckAt == nil || watch.NextCheckAt.After(now)) {
				return nil
			}
			if err != nil {
				return err
			}
			return w.Money.WatchBalanceInTx(ctx, d, identity.CustomerID(a.CustomerID), a.Currency)
		})
	})
}
