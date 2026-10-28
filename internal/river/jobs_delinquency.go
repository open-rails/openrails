package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/delinquency"
)

const (
	KindDelinquency = "openrails.delinquency"
	// DelinquencyInterval bounds how long a payer whose usage order fell due,
	// or whose grace ran out, waits to be evaluated: entering grace or
	// delinquency has no event.
	DelinquencyInterval = time.Minute

	// delinquencyBatch is how many due payers one read takes. The read is
	// indexed on the due instant, so a pass costs what is due.
	delinquencyBatch = 200
)

// DelinquencyArgs triggers one pass over the payers whose delinquency is due.
type DelinquencyArgs struct{}

func (DelinquencyArgs) Kind() string { return KindDelinquency }

func (DelinquencyArgs) InsertOpts() river.InsertOpts { return periodPass(DelinquencyInterval) }

// DelinquencyWorker evaluates the payers whose delinquency time alone changed
// (customer_delinquency.next_evaluation_at): a usage order fell due or a grace
// window ran out. A change to a usage order evaluates its payer inline. It
// moves no money and calls no provider, so it runs with no charger armed and
// in limited or readonly mode. A failed evaluation stays due and fails the
// pass once the others are done.
type DelinquencyWorker struct {
	WorkerDefaults[DelinquencyArgs]
	DB    *db.DB
	Clock clockwork.Clock
}

func (DelinquencyWorker) Kind() string { return KindDelinquency }

func (w DelinquencyWorker) now() time.Time {
	if w.Clock != nil {
		return w.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (w DelinquencyWorker) Work(ctx context.Context, _ *river.Job[DelinquencyArgs]) error {
	if w.DB == nil {
		return nil
	}
	logger := log.WithContext(ctx).WithField("worker", KindDelinquency)
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
				logger.WithError(err).WithFields(log.Fields{"merchant_id": row.MerchantID, "customer_id": row.CustomerID, "currency": row.Currency}).
					Error("delinquency: evaluation failed; continuing")
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
