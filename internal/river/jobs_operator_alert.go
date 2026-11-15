package riverjobs

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/alerting"
	"github.com/riverqueue/river"
)

// OperatorAlertWorker fans an operator alert out to one job per configured
// channel, each delivered and retried on its own.
type OperatorAlertWorker struct {
	WorkerDefaults[alerting.DeliveryArgs]
	DB     *db.DB
	Alerts *alerting.Service
}

func (w *OperatorAlertWorker) Work(ctx context.Context, job *river.Job[alerting.DeliveryArgs]) error {
	if w.DB == nil || w.Alerts == nil {
		return fmt.Errorf("operator alert worker is not wired")
	}
	a := job.Args
	if !a.Valid() {
		return river.JobCancel(fmt.Errorf("operator alert names no merchant and source"))
	}
	mid := billing.MerchantID(a.MerchantID)
	ctx = merchant.WithID(ctx, mid)
	if a.Channel != nil {
		return w.Alerts.Deliver(ctx, a)
	}
	channels, err := w.Alerts.Channels(ctx, a)
	if err != nil || len(channels) == 0 {
		return err
	}
	return w.DB.DataPool().CommittedMerchantTx(ctx, mid, func(ctx context.Context, tx pgx.Tx) error {
		for _, channel := range channels {
			next := a
			next.Channel = &channel
			if _, err := w.DB.InsertRiverJobTx(ctx, tx, next, nil); err != nil {
				return err
			}
		}
		return nil
	})
}
