package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// CardRefreshWorker reads a card whose charge was declined as expired or
// reissued. A newer card held for it applies to the same method and wakes the
// subscriptions waiting on it; otherwise the customer is asked for a new card.
type CardRefreshWorker struct {
	river.WorkerDefaults[paymentmethods.CardRefreshArgs]
	DB        *db.DB
	Clock     clockwork.Clock
	Holders   paymentmethods.CardHolders
	Lifecycle *subscriptions.SubscriptionLifecycleService
}

func (CardRefreshWorker) Kind() string { return paymentmethods.KindCardRefresh }

func (w *CardRefreshWorker) Work(ctx context.Context, job *river.Job[paymentmethods.CardRefreshArgs]) error {
	if w.DB == nil || w.Holders == nil || w.Lifecycle == nil {
		return errors.New("card refresh: not wired")
	}
	args := job.Args
	return w.DB.RunInMerchantScope(ctx, billing.MerchantID(args.MerchantID), "card refresh", func(ctx context.Context) error {
		method, err := w.DB.Gen(ctx).GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: args.MerchantID, ID: args.PaymentMethodID})
		if db.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		held, err := w.Holders.ReadHeldCard(ctx, method)
		if err != nil {
			if job.Attempt < paymentmethods.CardRefreshAttempts {
				return fmt.Errorf("card refresh: read holder: %w", err)
			}
			log.WithContext(ctx).WithError(err).WithField("payment_method_id", method.ID).Warn("card refresh: holder unreadable; asking the customer")
			held = paymentmethods.HeldCard{}
		}
		now := w.Clock.Now().UTC()
		var notices []*models.NotificationQueue
		err = w.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			d := w.DB.NewWithPgxTx(tx)
			notices = nil
			if held.Source != "" && !held.Gone {
				life, queued, err := w.Lifecycle.ApplyCardLifecycle(ctx, d, paymentmethods.CardEvent{MerchantID: args.MerchantID, PaymentMethodID: method.ID,
					Source: held.Source, EventRef: "decline-read:" + strconv.FormatInt(job.ID, 10), Card: held.Card, At: now})
				if err != nil || life.Replayed || life.Change != "" {
					notices = queued
					return err
				}
			}
			var err error
			notices, err = w.Lifecycle.AskForCard(ctx, d, args.MerchantID, method.ID, now)
			return err
		})
		if err != nil {
			return err
		}
		w.Lifecycle.DispatchNotifications(ctx, notices)
		return nil
	})
}
