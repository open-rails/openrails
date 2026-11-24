package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/modules/delinquency"
)

const (
	KindDelinquency = "openrails.delinquency"

	// delinquencyBatch is how many due payers one read takes. The read is
	// indexed on the due instant, so a pass costs what is due.
	delinquencyBatch = 200
)

// DelinquencyWorker evaluates the payers whose delinquency time alone changed
// (customer_delinquency.next_evaluation_at): a usage order fell due or a grace
// window ran out. A change to a usage order evaluates its payer inline. It
// moves no money and calls no provider, so it runs with no charger armed and
// under a limited or readonly write posture. A failed evaluation stays due and fails the
// pass once the others are done.
type DelinquencyWorker struct {
	DB    *db.DB
	Clock clockwork.Clock
	Log   *slog.Logger
}

func (w DelinquencyWorker) now() time.Time {
	if w.Clock != nil {
		return w.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

// DueBy reports a payer the pass would evaluate at at.
func (w DelinquencyWorker) DueBy(ctx context.Context, at time.Time) (bool, error) {
	due, err := w.DB.GenDirectory().ListDueDelinquency(ctx, gen.ListDueDelinquencyParams{Now: at, RowLimit: 1})
	return len(due) > 0, err
}

func (w DelinquencyWorker) Pass(ctx context.Context) error {
	if w.DB == nil {
		return nil
	}
	logger := w.Log.With("worker", KindDelinquency)
	svc := delinquency.NewService(w.DB, w.Clock)
	args := gen.ListDueDelinquencyParams{Now: w.now(), RowLimit: delinquencyBatch}
	var failed error
	for {
		due, err := w.DB.GenDirectory().ListDueDelinquency(ctx, args)
		if err != nil {
			return errors.Join(failed, fmt.Errorf("list due delinquency: %w", err))
		}
		for _, row := range due {
			err := w.DB.RunInMerchantScope(ctx, billing.MerchantID(row.MerchantID), "delinquency evaluation", func(ctx context.Context) error {
				_, _, err := svc.Evaluate(ctx, row.CustomerID, row.Currency)
				return err
			})
			if err != nil {
				logger.ErrorContext(ctx, "delinquency: evaluation failed; continuing", logx.Err(err), "merchant_id", row.MerchantID, "customer_id", row.CustomerID, "currency", row.Currency)
				failed = errors.Join(failed, fmt.Errorf("delinquency: merchant %s customer %s %s: %w", row.MerchantID, row.CustomerID, row.Currency, err))
			}
		}
		if len(due) < delinquencyBatch {
			return failed
		}
		last := due[len(due)-1]
		args.AfterAt, args.AfterMerchantID, args.AfterCustomerID, args.AfterCurrency = &last.NextEvaluationAt, &last.MerchantID, &last.CustomerID, &last.Currency
	}
}
