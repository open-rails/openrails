package app

import (
	"context"
	"errors"
	"reflect"

	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantdocs"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// followStripeWebhooks queues the webhook endpoint pass of each Stripe PSP a
// configuration change declared or changed, so Stripe reaches it at once. A
// PSP this process loads for the first time is queued only when its endpoint
// was not settled at this boot's configuration (StripeWebhookDigest).
func (r *Runtime) followStripeWebhooks(mid billing.MerchantID, previous, current merchantdocs.Set) {
	if r.DB == nil {
		return
	}
	var changed, loaded []string
	for key, doc := range current.PSPs {
		p := doc.Value
		if p.Rail != string(models.RailStripe) || p.Archived {
			continue
		}
		if _, rejected := current.Rejected[merchantdocs.PSPDoc(key)]; rejected {
			continue
		}
		before, ok := previous.PSPs[key]
		switch {
		case !ok:
			loaded = append(loaded, p.AccountID)
		case !reflect.DeepEqual(before.Value, p):
			changed = append(changed, p.AccountID)
		}
	}
	if len(changed)+len(loaded) == 0 {
		return
	}
	r.Go("stripe webhook registration", func(ctx context.Context) {
		ctx = merchant.WithID(ctx, mid)
		err := r.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			accounts := changed
			if len(loaded) > 0 {
				settled, err := r.DB.NewWithPgxTx(tx).Gen(ctx).ListPSPWebhookDigests(ctx, gen.ListPSPWebhookDigestsParams{MerchantID: mid.UUID(), Rail: string(models.RailStripe)})
				if err != nil {
					return err
				}
				at := map[string]string{}
				for _, s := range settled {
					if s.WebhookDigest != nil {
						at[s.AccountID] = *s.WebhookDigest
					}
				}
				for _, account := range loaded {
					if at[account] != riverjobs.StripeWebhookDigest(r.Config, account) {
						accounts = append(accounts, account)
					}
				}
			}
			for _, account := range accounts {
				if err := riverjobs.QueueStripeWebhook(ctx, r.DB, tx, mid, account); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil && !errors.Is(err, db.ErrNoRiverProducer) && ctx.Err() == nil {
			log.WithContext(ctx).WithError(err).WithField("merchant_id", mid.String()).Warn("stripe webhook: cannot queue the endpoint pass; the daily pass registers it")
		}
	})
}
