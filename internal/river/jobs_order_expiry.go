package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/orders"
	"github.com/open-rails/openrails/internal/modules/payment"
)

const KindOrderExpiry = "openrails.order_expiry"

// OrderExpiryWorker expires orders past their expiry, releasing their claims,
// and the order attempts still waiting on their customer past theirs: it is
// their one expiry writer. It never touches an order whose payment is
// processing: only the provider's answer ends that. It also removes card
// saves left waiting for the bank's authentication.
type OrderExpiryWorker struct {
	DB     *db.DB
	Orders *orders.Service
	Clock  clockwork.Clock
}

// orderExpiryBatch is how many merchants, orders or attempts one read takes.
// Each read is indexed on the expiry, so a pass costs what is due.
const orderExpiryBatch = 200

// DueBy reports an order or an order attempt past its expiry since before at.
func (w OrderExpiryWorker) DueBy(ctx context.Context, at time.Time) (bool, error) {
	due, err := orders.ExpiryMerchants(ctx, w.DB, at, uuid.Nil, 1)
	return len(due) > 0, err
}

// Pass pages through the merchants with something due and drains each, within
// the pass's budget; what the budget leaves stays due for the next pass.
func (w OrderExpiryWorker) Pass(ctx context.Context) error {
	if w.Orders == nil {
		return nil
	}
	now := workerNow(w.Clock)
	// One merchant's failure never stops the others; the job reports them all.
	var workErr error
	for after := uuid.Nil; ; {
		merchants, err := orders.ExpiryMerchants(ctx, w.DB, now, after, orderExpiryBatch)
		if err != nil {
			return errors.Join(workErr, fmt.Errorf("order expiry: list merchants: %w", err))
		}
		for _, id := range merchants {
			if err := ctx.Err(); err != nil {
				return errors.Join(workErr, fmt.Errorf("order expiry: %w", err))
			}
			if err := w.DB.RunInMerchantScope(ctx, billing.MerchantID(id), "order expiry sweep", func(ctx context.Context) error {
				if _, err := w.Orders.ExpireDue(ctx, orderExpiryBatch); err != nil {
					return err
				}
				for {
					n, err := w.DB.Gen(ctx).ExpireOrderAttempts(ctx, gen.ExpireOrderAttemptsParams{MerchantID: id, Now: now, RowLimit: orderExpiryBatch})
					if err != nil || n < orderExpiryBatch {
						return err
					}
					if err := ctx.Err(); err != nil {
						return err
					}
				}
			}); err != nil {
				workErr = errors.Join(workErr, fmt.Errorf("order expiry: merchant %s: %w", id, err))
			}
		}
		if len(merchants) < orderExpiryBatch {
			break
		}
		after = merchants[len(merchants)-1]
	}
	before := now.Add(-payment.SetupAbandonAfter)
	for after := uuid.Nil; ; {
		stale, err := w.DB.GenDirectory().ListStaleSetupMerchants(ctx, gen.ListStaleSetupMerchantsParams{Before: before, Now: now, AfterMerchantID: after, MerchantLimit: orderExpiryBatch})
		if err != nil {
			return errors.Join(workErr, fmt.Errorf("card save expiry: list merchants: %w", err))
		}
		for _, id := range stale {
			if err := ctx.Err(); err != nil {
				return errors.Join(workErr, fmt.Errorf("card save expiry: %w", err))
			}
			if err := w.DB.RunInMerchantScope(ctx, billing.MerchantID(id), "card save expiry", func(ctx context.Context) error {
				_, err := w.DB.Gen(ctx).AbandonStalePaymentMethodSetups(ctx, gen.AbandonStalePaymentMethodSetupsParams{MerchantID: id, Before: before, Now: now})
				return err
			}); err != nil {
				workErr = errors.Join(workErr, fmt.Errorf("card save expiry: merchant %s: %w", id, err))
			}
		}
		if len(stale) < orderExpiryBatch {
			return workErr
		}
		after = stale[len(stale)-1]
	}
}
