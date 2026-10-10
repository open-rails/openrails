package riverjobs

import (
	"context"
	"fmt"

	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/modules/orders"
	"github.com/open-rails/openrails/internal/shared/progress"
)

const KindOrderExpiry = "openrails.order_expiry"

type OrderExpiryArgs struct{}

func (OrderExpiryArgs) Kind() string { return KindOrderExpiry }

// OrderExpiryWorker expires orders past their expiry, releasing their claims,
// and purges unpaid closed orders past retention (#1168). It never touches an
// order whose payment is processing: only the provider's answer ends that.
type OrderExpiryWorker struct {
	river.WorkerDefaults[OrderExpiryArgs]
	DB     *db.DB
	Orders *orders.Service
	Clock  clockwork.Clock
}

func (OrderExpiryWorker) Kind() string { return KindOrderExpiry }

const orderExpiryBatch = 200

func (w OrderExpiryWorker) Work(ctx context.Context, _ *river.Job[OrderExpiryArgs]) error {
	if w.Orders == nil {
		return nil
	}
	merchants, err := orders.SweepMerchants(ctx, w.DB, workerNow(w.Clock), orderExpiryBatch)
	if err != nil {
		return fmt.Errorf("order expiry: list merchants: %w", err)
	}
	for _, id := range merchants {
		progress.Mark(ctx, "order expiry merchant "+id.String())
		if err := w.DB.RunInMerchantScope(ctx, billing.MerchantID(id), "order expiry sweep", func(ctx context.Context) error {
			_, err := w.Orders.ExpireDue(ctx, orderExpiryBatch)
			return err
		}); err != nil {
			log.WithContext(ctx).WithError(err).WithField("merchant_id", id).Error("order expiry: merchant pass failed; continuing")
		}
	}
	return nil
}
