package riverjobs

import (
	"context"
	"fmt"

	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/convergejob"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/alerting"
	"github.com/open-rails/openrails/internal/reconcile/converge"
	"github.com/open-rails/openrails/internal/writeposture"
)

// CustomerConvergeWorker converges one customer after a mutation that changed
// it: its grant effects, references, duplicate charges and access-ended
// notice, closing the findings that cleared. Its job commits with the
// mutation (convergejob.Queue).
type CustomerConvergeWorker struct {
	WorkerDefaults[convergejob.Args]
	DB     *db.DB
	Config *config.Config
	Clock  clockwork.Clock
	// Alerts sends requires_review findings to the operator. nil = none.
	Alerts *alerting.Service
}

func (CustomerConvergeWorker) Kind() string { return convergejob.Kind }

func (w CustomerConvergeWorker) Work(ctx context.Context, job *river.Job[convergejob.Args]) error {
	mid, customer := billing.MerchantID(job.Args.MerchantID), job.Args.CustomerID
	engine := converge.NewConvergeEngine(w.DB, w.Clock)
	if w.Alerts != nil {
		engine.Notifier = w.Alerts
	}
	postures := writeposture.View{Config: w.Config, DB: w.DB}
	return w.DB.RunInMerchantConn(merchant.WithID(ctx, mid), func(ctx context.Context) error {
		// A readonly merchant is observed, never written.
		if postures.Posture(ctx, mid.UUID()).ReadOnly() {
			return nil
		}
		if _, err := engine.Converge(ctx, converge.Scope{Merchant: mid, Customer: &customer}); err != nil {
			return fmt.Errorf("converge customer %s: %w", customer, err)
		}
		return nil
	})
}
