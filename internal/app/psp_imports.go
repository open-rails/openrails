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
	"github.com/open-rails/openrails/internal/proposals"
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
		CCBillDataLinkURL: config.SandboxCCBillDataLinkURL(r.Config), Directory: r.Contacts, Proposals: proposals.New(r.DB, r.Config, r.Clock), Clock: r.Clock}
	if r.AlertService != nil {
		im.Alerts = r.AlertService
	}
	return im
}

// EnsurePSPOnboarding gives each live PSP of the merchant its onboarding
// import now, restarting a failed one, as a process's first load of its
// configuration does in the background. It reads the published configuration,
// loading it first if needed. A tool that reads the onboarding import
// afterwards sees its latest state or the restart.
func (r *Runtime) EnsurePSPOnboarding(ctx context.Context, mid billing.MerchantID) error {
	if r == nil || r.DB == nil || r.Merchants == nil {
		return errors.New("psp onboarding: merchant configuration is not wired")
	}
	ctx = merchant.WithID(ctx, mid)
	scopes, err := r.Merchants.LivePSPScopes(ctx, mid)
	if err != nil {
		return err
	}
	rearm := make(map[uuid.UUID]bool, len(scopes))
	for _, scope := range scopes {
		rearm[scope.ID] = true
	}
	return r.PSPImporter().EnsureOnboarding(ctx, mid, scopes, rearm)
}

// followPSPImports gives the merchant's PSPs their onboarding imports when a
// PSP document is new to this process or changed, in the background and
// retried until the database answers. It reads the PSPs from current, the set
// being loaded: the cache publishes it only once this load returns. A failed
// onboarding import of such a PSP restarts. The pull only reads, so read-only
// provider writes do not hold it.
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
			scopes, err := r.Merchants.LivePSPScopesIn(ctx, mid, current)
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
