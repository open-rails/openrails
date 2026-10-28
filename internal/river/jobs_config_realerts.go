package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/internal/modules/alerting"
	"github.com/open-rails/openrails/internal/reconcile"
)

const (
	KindConfigRealerts = "openrails.config_realerts"
	// ConfigRealertInterval is how often due re-alerts are looked for.
	ConfigRealertInterval = time.Hour
	// ConfigRealert is how long an unresolved undeclared account waits after
	// its last alert before it alerts again.
	ConfigRealert = 24 * time.Hour

	configRealertMerchants = 500
)

// ConfigRealertArgs is one pass over due configuration re-alerts.
type ConfigRealertArgs struct{}

func (ConfigRealertArgs) Kind() string { return KindConfigRealerts }

func (ConfigRealertArgs) InsertOpts() river.InsertOpts { return periodPass(ConfigRealertInterval) }

// ConfigRealertWorker alerts each merchant's operator again of every
// undeclared account still unresolved ConfigRealert after its last alert. The
// merchant's configuration is re-evaluated first, which resolves what a
// declaration or an ended subscription settled; then the due findings are
// re-armed and claimed in one statement and delivered.
type ConfigRealertWorker struct {
	WorkerDefaults[ConfigRealertArgs]
	DB    *db.DB
	Clock clockwork.Clock
	// Reload re-evaluates a merchant's configuration.
	Reload func(context.Context, billing.MerchantID) error
	// Alerts delivers the re-alerts; nil claims none.
	Alerts *alerting.Service
}

func (ConfigRealertWorker) Kind() string { return KindConfigRealerts }

func (w ConfigRealertWorker) Work(ctx context.Context, _ *river.Job[ConfigRealertArgs]) error {
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
		var due []gen.BillingReconciliationFinding
		err := w.DB.RunInMerchantScope(ctx, mid, "config re-alerts", func(ctx context.Context) error {
			var err error
			due, err = w.DB.Gen(ctx).ClaimConfigFindingRealerts(ctx, gen.ClaimConfigFindingRealertsParams{
				MerchantID: id, FindingType: merchantdocs.UndeclaredAccountFindingType, NotifiedBefore: before, Now: w.Clock.Now().UTC()})
			if err != nil {
				return err
			}
			for _, f := range due {
				if err := w.Alerts.RealertFinding(ctx, reconcile.FindingRecordFromRow(f)); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("merchant %s: %w", mid, err))
		}
	}
	return errs
}
