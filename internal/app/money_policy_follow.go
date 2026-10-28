package app

import (
	"context"
	"reflect"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchantdocs"
)

// followMoneyPolicy marks the due work a change to a merchant's settings may
// move: its payers in grace or delinquent are evaluated again. A first load
// counts as a change, since the settings may have changed while no process
// served the merchant.
func (r *Runtime) followMoneyPolicy(mid billing.MerchantID, previous, current merchantdocs.Set) {
	if r.DB == nil || !current.HasMerchant {
		return
	}
	if _, rejected := current.Rejected[merchantdocs.MerchantDoc]; rejected {
		return
	}
	if previous.HasMerchant && reflect.DeepEqual(previous.Merchant.Value.MerchantSettings, current.Merchant.Value.MerchantSettings) {
		return
	}
	r.Go("money policy follow", func(ctx context.Context) {
		err := r.DB.RunInMerchantScope(ctx, mid, "money policy follow", func(ctx context.Context) error {
			return r.DB.Gen(ctx).MarkDelinquencyDue(ctx, gen.MarkDelinquencyDueParams{MerchantID: mid.UUID(), Now: r.Clock.Now().UTC()})
		})
		if err != nil && ctx.Err() == nil {
			log.WithContext(ctx).WithError(err).WithField("merchant_id", mid.String()).Warn("money policy: cannot mark the merchant's due work")
		}
	})
}
