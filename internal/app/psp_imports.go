package app

import (
	"context"
	"errors"
	"reflect"
	"strings"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/internal/modules/catalog/providerobjects"
	"github.com/open-rails/openrails/internal/pspimport"
	"github.com/open-rails/openrails/internal/retry"
)

// ProviderObjects observes the merchants' PSP catalogs with the runtime's
// current merchants and clients.
func (r *Runtime) ProviderObjects() *providerobjects.Observer {
	if r == nil || r.DB == nil {
		return nil
	}
	return &providerobjects.Observer{DB: r.DB, Merchants: r.Merchants, NMI: r.NMIClients, Stripe: r.StripeClients, Clock: r.Clock}
}

// PSPImporter runs PSP imports over the runtime's current merchants, NMI
// clients and directory.
func (r *Runtime) PSPImporter() *pspimport.Importer {
	if r == nil || r.DB == nil {
		return nil
	}
	im := &pspimport.Importer{DB: r.DB, Merchants: r.Merchants, NMI: r.NMIClients, Stripe: r.StripeClients, Objects: r.ProviderObjects(),
		CCBillDataLinkURL: config.SandboxCCBillDataLinkURL(r.Config), Directory: r.Contacts, Clock: r.Clock}
	if r.AlertService != nil {
		im.Alerts = r.AlertService
	}
	return im
}

// followPSPImports gives the merchant's PSPs their onboarding imports when a
// PSP document is new to this process or changed: in the background, since
// the loaded set is published only once this load returns, and retried until
// the database answers. A failed onboarding import of such a PSP restarts.
// The pull only reads, so read-only provider writes do not hold it.
func (r *Runtime) followPSPImports(mid billing.MerchantID, previous, current merchantdocs.Set) {
	if r == nil || r.DB == nil || r.Merchants == nil {
		return
	}
	changed := map[string]bool{}
	for key, doc := range current.PSPs {
		if before, ok := previous.PSPs[key]; !ok || !reflect.DeepEqual(before.Value, doc.Value) {
			changed[key] = true
		}
	}
	for key := range previous.PSPs {
		if _, ok := current.PSPs[key]; !ok {
			changed[key] = true
		}
	}
	if len(changed) == 0 {
		return
	}
	r.Go("psp onboarding imports", func(ctx context.Context) {
		ctx = merchant.WithID(ctx, mid)
		_ = retry.Forever(ctx, func(ctx context.Context) error {
			scopes, err := r.Merchants.LivePSPScopes(ctx, mid)
			if err != nil {
				return err
			}
			rearm := map[uuid.UUID]bool{}
			for _, scope := range scopes {
				if changed[strings.ToLower(scope.Key)] {
					rearm[scope.ID] = true
				}
			}
			err = r.PSPImporter().EnsureOnboarding(ctx, mid, scopes, rearm)
			if errors.Is(err, db.ErrNoRiverProducer) {
				log.WithContext(ctx).WithField("merchant_id", mid.String()).Warn("psp onboarding imports: no job runner; a process that has one starts them")
				return nil
			}
			return err
		}, func(attempt int, err error) {
			log.WithContext(ctx).WithError(err).WithField("merchant_id", mid.String()).WithField("attempt", attempt).Warn("psp onboarding imports: retrying")
		})
	})
}
