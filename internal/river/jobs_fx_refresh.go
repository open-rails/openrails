package riverjobs

import (
	"context"

	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/internal/integrations/fx"
)

const KindFXRefresh = "openrails.fx_refresh"

type FXRefreshArgs struct{}

func (FXRefreshArgs) Kind() string { return KindFXRefresh }

// FXRefreshWorker reads every currency's FX rates for the whole fleet: River's
// leader alone schedules it, and every replica quotes what it stored.
type FXRefreshWorker struct {
	river.WorkerDefaults[FXRefreshArgs]
	Rates *fx.Rates
}

func (FXRefreshWorker) Kind() string { return KindFXRefresh }

func (w FXRefreshWorker) Work(ctx context.Context, _ *river.Job[FXRefreshArgs]) error {
	if w.Rates == nil {
		return nil
	}
	return w.Rates.Refresh(ctx)
}
