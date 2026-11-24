package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/logx"
)

const (
	KindMRRDue = "openrails.mrr_due"

	// mrrDueBatch is how many due subscriptions one read takes. The read is
	// indexed on subscription_mrr.next_change_at, so a pass costs what is
	// due, never what is subscribed.
	mrrDueBatch = 200
)

// MRRDueWorker takes the subscriptions whose MRR time alone changes: one
// canceled at period end leaves MRR when its end comes, dated at that end.
// Then it re-evaluates the risk of the customers a change of a card, a default
// card, a provider's decline or delinquency queued. Each runs in its
// merchant's scope; one that fails stays due and fails the pass once the
// others are done.
type MRRDueWorker struct {
	DB    *db.DB
	Clock clockwork.Clock
	Log   *slog.Logger
}

func (w *MRRDueWorker) Pass(ctx context.Context) error {
	now := workerNow(w.Clock)
	logger := w.Log.With("worker", KindMRRDue)
	args := gen.ListDueSubscriptionMRRParams{At: now, RowLimit: mrrDueBatch}
	var failed error
	for {
		due, err := w.DB.GenDirectory().ListDueSubscriptionMRR(ctx, args)
		if err != nil {
			return errors.Join(failed, err)
		}
		for _, row := range due {
			err := w.DB.RunInMerchantScope(ctx, billing.MerchantID(row.MerchantID), "mrr due", func(ctx context.Context) error {
				_, err := w.DB.Gen(ctx).EffectDueSubscriptionMRR(ctx, gen.EffectDueSubscriptionMRRParams{MerchantID: row.MerchantID, SubscriptionID: row.SubscriptionID, At: now})
				return err
			})
			if err != nil {
				logger.ErrorContext(ctx, "mrr due: failed; continuing", logx.Err(err), "merchant_id", row.MerchantID, "subscription_id", row.SubscriptionID)
				failed = errors.Join(failed, fmt.Errorf("mrr due: merchant %s subscription %s: %w", row.MerchantID, row.SubscriptionID, err))
			}
			if ctx.Err() != nil {
				return errors.Join(failed, ctx.Err())
			}
		}
		if len(due) < mrrDueBatch {
			break
		}
		last := due[len(due)-1]
		args.AfterAt, args.AfterMerchantID, args.AfterSubscriptionID = &last.NextChangeAt, &last.MerchantID, &last.SubscriptionID
	}
	return errors.Join(failed, w.risk(ctx, logger))
}

// risk re-evaluates the queued customers, each in its merchant's scope.
func (w *MRRDueWorker) risk(ctx context.Context, logger *slog.Logger) error {
	args := gen.ListDueMRRRiskParams{RowLimit: mrrDueBatch}
	var failed error
	for {
		due, err := w.DB.GenDirectory().ListDueMRRRisk(ctx, args)
		if err != nil {
			return errors.Join(failed, err)
		}
		for _, row := range due {
			err := w.DB.RunInMerchantScope(ctx, billing.MerchantID(row.MerchantID), "mrr risk due", func(ctx context.Context) error {
				return w.DB.Gen(ctx).EffectDueMRRRisk(ctx, gen.EffectDueMRRRiskParams{MerchantID: row.MerchantID, CustomerID: row.CustomerID, ChangedAt: row.ChangedAt})
			})
			if err != nil {
				logger.ErrorContext(ctx, "mrr risk due: failed; continuing", logx.Err(err), "merchant_id", row.MerchantID, "customer_id", row.CustomerID)
				failed = errors.Join(failed, fmt.Errorf("mrr risk due: merchant %s customer %s: %w", row.MerchantID, row.CustomerID, err))
			}
			if ctx.Err() != nil {
				return errors.Join(failed, ctx.Err())
			}
		}
		if len(due) < mrrDueBatch {
			return failed
		}
		last := due[len(due)-1]
		args.AfterMerchantID, args.AfterCustomerID = &last.MerchantID, &last.CustomerID
	}
}
