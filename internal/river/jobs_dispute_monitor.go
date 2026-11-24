package riverjobs

import (
	"context"
	"fmt"

	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/disputes"
)

// DisputeMonitorWorker evaluates one PSP's dispute monitoring programs after
// a chargeback or a fraud report committed, raising and alerting the
// findings at their warning and excessive levels.
type DisputeMonitorWorker struct {
	WorkerDefaults[disputes.MonitorArgs]
	DB       *db.DB
	Disputes *disputes.Service
}

func (DisputeMonitorWorker) Kind() string { return disputes.MonitorKind }

func (w DisputeMonitorWorker) Work(ctx context.Context, job *river.Job[disputes.MonitorArgs]) error {
	if w.Disputes == nil {
		return nil
	}
	mid := billing.MerchantID(job.Args.MerchantID)
	return w.DB.RunInMerchantConn(merchant.WithID(ctx, mid), func(ctx context.Context) error {
		if err := w.Disputes.Evaluate(ctx, job.Args.PSPID); err != nil {
			return fmt.Errorf("evaluate dispute programs of PSP %s: %w", billing.PSPID(job.Args.PSPID), err)
		}
		return nil
	})
}
