package app

import (
	"context"
	"errors"
	"reflect"

	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchantdocs"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// followStripeWebhooks queues the webhook endpoint pass of each Stripe PSP a
// configuration change declared or changed, so Stripe reaches it at once
// rather than at the next hourly pass. Before the host's River client is
// bound, the hourly pass on start registers it.
func (r *Runtime) followStripeWebhooks(mid billing.MerchantID, previous, current merchantdocs.Set) {
	if r.DB == nil {
		return
	}
	var accounts []string
	for key, doc := range current.PSPs {
		p := doc.Value
		if p.Rail != string(models.RailStripe) || p.Archived {
			continue
		}
		if _, rejected := current.Rejected[merchantdocs.PSPDoc(key)]; rejected {
			continue
		}
		if before, ok := previous.PSPs[key]; ok && reflect.DeepEqual(before.Value, p) {
			continue
		}
		accounts = append(accounts, p.AccountID)
	}
	if len(accounts) == 0 {
		return
	}
	r.Go("stripe webhook registration", func(ctx context.Context) {
		err := r.DB.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			for _, account := range accounts {
				args := riverjobs.StripeWebhookReconcileArgs{MerchantID: mid.UUID(), AccountID: account}
				opts := args.InsertOpts()
				if _, err := r.DB.InsertRiverJobTx(ctx, tx, args, &opts); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil && !errors.Is(err, db.ErrNoRiverProducer) && ctx.Err() == nil {
			log.WithContext(ctx).WithError(err).WithField("merchant_id", mid.String()).Warn("stripe webhook: cannot queue the endpoint pass; the hourly pass registers it")
		}
	})
}
