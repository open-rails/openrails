package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/internal/modules/alerting"
)

const (
	KindConfigRealerts = "openrails.config_realerts"
	// ConfigRealert is how long an unresolved undeclared account waits after
	// its last alert before it alerts again.
	ConfigRealert = 24 * time.Hour

	configRealertMerchants = 500
)

// ConfigRealertWorker alerts each merchant's operator again of every
// undeclared account still unresolved ConfigRealert after its last alert. The
// merchant's configuration is re-evaluated first, which resolves what a
// declaration or an ended subscription settled; then the due findings are
// re-armed and claimed in one statement, committed with their alerts.
type ConfigRealertWorker struct {
	DB    *db.DB
	Clock clockwork.Clock
	// Reload re-evaluates a merchant's configuration.
	Reload func(context.Context, billing.MerchantID) error
	// Alerts queues the re-alerts; nil claims none.
	Alerts *alerting.Service
}

func (w ConfigRealertWorker) Pass(ctx context.Context) error {
	if w.Alerts == nil {
		return nil
	}
	before := w.Clock.Now().UTC().Add(-ConfigRealert)
	merchants, err := w.DB.GenDirectory().ListConfigRealertMerchants(ctx, gen.ListConfigRealertMerchantsParams{
		FindingType: merchantdocs.UndeclaredAccountFindingType, NotifiedBefore: before, MerchantLimit: configRealertMerchants})
	if err != nil {
		return fmt.Errorf("config re-alerts: list merchants: %w", err)
	}
	var errs error
	for _, id := range merchants {
		mid := billing.MerchantID(id)
		if w.Reload != nil {
			if err := w.Reload(ctx, mid); err != nil {
				errs = errors.Join(errs, fmt.Errorf("merchant %s: reload configuration: %w", mid, err))
				continue
			}
		}
		err := w.DB.RunInMerchantScope(ctx, mid, "config re-alerts", func(ctx context.Context) error {
			return w.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				due, err := w.DB.NewWithPgxTx(tx).Gen(ctx).ClaimConfigFindingRealerts(ctx, gen.ClaimConfigFindingRealertsParams{
					MerchantID: id, FindingType: merchantdocs.UndeclaredAccountFindingType, NotifiedBefore: before, Now: w.Clock.Now().UTC()})
				if err != nil {
					return err
				}
				return w.Alerts.QueueRealerts(ctx, tx, due)
			})
		})
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("merchant %s: %w", mid, err))
		}
	}
	return errs
}
