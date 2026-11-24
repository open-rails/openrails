package riverjobs

import (
	"context"
	"fmt"
	"time"

	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

const (
	// KindMRRRisk drains the queued MRR risk changes: the backstop for a
	// drain that missed its wake.
	KindMRRRisk = "openrails.mrr_risk"
	// MRRRiskInterval is the backstop's cadence.
	MRRRiskInterval = 30 * time.Second
)

// MRRRiskArgs is one backstop drain.
type MRRRiskArgs struct{}

func (MRRRiskArgs) Kind() string { return KindMRRRisk }

// InsertOpts: one attempt and one drain per period; the next retries.
func (MRRRiskArgs) InsertOpts() river.InsertOpts { return periodPass(MRRRiskInterval) }

// MRRRiskWorker re-evaluates the risk of the customers queued while no drain
// was woken.
type MRRRiskWorker struct {
	WorkerDefaults[MRRRiskArgs]
	Risk *subscriptions.MRRRisk
}

func (w *MRRRiskWorker) Work(ctx context.Context, _ *river.Job[MRRRiskArgs]) error {
	if w.Risk == nil {
		return fmt.Errorf("mrr risk worker is not wired")
	}
	return w.Risk.Drain(ctx)
}
