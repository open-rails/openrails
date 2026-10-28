package riverjobs

import (
	"context"
	"errors"
	"fmt"

	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/orders"
	"github.com/open-rails/openrails/internal/modules/payment"
)

const KindOrderExpiry = "openrails.order_expiry"

type OrderExpiryArgs struct{}

func (OrderExpiryArgs) Kind() string { return KindOrderExpiry }

// OrderExpiryWorker expires orders past their expiry, releasing their claims,
// and the order attempts still waiting on their customer past theirs: it is
// their one expiry writer. It never touches an order whose payment is
// processing: only the provider's answer ends that. It also removes card
// saves left waiting for the bank's authentication.
type OrderExpiryWorker struct {
	WorkerDefaults[OrderExpiryArgs]
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
	now := workerNow(w.Clock)
	merchants, err := orders.ExpiryMerchants(ctx, w.DB, now, orderExpiryBatch)
	if err != nil {
		return fmt.Errorf("order expiry: list merchants: %w", err)
	}
	// One merchant's failure never stops the others; the job reports them all.
	var workErr error
	for _, id := range merchants {
		if err := w.DB.RunInMerchantScope(ctx, billing.MerchantID(id), "order expiry sweep", func(ctx context.Context) error {
			if _, err := w.Orders.ExpireDue(ctx, orderExpiryBatch); err != nil {
				return err
			}
			_, err := w.DB.Gen(ctx).ExpireOrderAttempts(ctx, gen.ExpireOrderAttemptsParams{MerchantID: id, Now: now, RowLimit: orderExpiryBatch})
			return err
		}); err != nil {
			workErr = errors.Join(workErr, fmt.Errorf("order expiry: merchant %s: %w", id, err))
		}
	}
	before := now.Add(-payment.SetupAbandonAfter)
	stale, err := w.DB.GenDirectory().ListStaleSetupMerchants(ctx, gen.ListStaleSetupMerchantsParams{Before: before, Now: now, MerchantLimit: orderExpiryBatch})
	if err != nil {
		return errors.Join(workErr, fmt.Errorf("card save expiry: list merchants: %w", err))
	}
	for _, id := range stale {
		if err := w.DB.RunInMerchantScope(ctx, billing.MerchantID(id), "card save expiry", func(ctx context.Context) error {
			_, err := w.DB.Gen(ctx).AbandonStalePaymentMethodSetups(ctx, gen.AbandonStalePaymentMethodSetupsParams{MerchantID: id, Before: before, Now: now})
			return err
		}); err != nil {
			workErr = errors.Join(workErr, fmt.Errorf("card save expiry: merchant %s: %w", id, err))
		}
	}
	return workErr
}
