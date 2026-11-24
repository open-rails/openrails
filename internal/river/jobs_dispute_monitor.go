package riverjobs

import (
	"context"
	"fmt"

	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/disputejob"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/disputes"
)

// DisputeMonitorWorker evaluates one PSP's dispute monitoring programs after
// a chargeback, a fraud report or a sale while a finding is open, raising,
// alerting and resolving the findings at their warning and excessive levels.
type DisputeMonitorWorker struct {
	WorkerDefaults[disputejob.MonitorArgs]
	DB       *db.DB
	Disputes *disputes.Service
}

func (DisputeMonitorWorker) Kind() string { return disputejob.MonitorKind }

func (w DisputeMonitorWorker) Work(ctx context.Context, job *river.Job[disputejob.MonitorArgs]) error {
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

// DisputeResponseWorker carries out what one chargeback or fraud report calls
// for. A response a gate holds is left waiting with its finding; the change
// that releases it queues it again.
type DisputeResponseWorker struct {
	WorkerDefaults[disputejob.ResponseArgs]
	DB       *db.DB
	Disputes *disputes.Service
}

func (DisputeResponseWorker) Kind() string { return disputejob.ResponseKind }

func (w DisputeResponseWorker) Work(ctx context.Context, job *river.Job[disputejob.ResponseArgs]) error {
	if w.Disputes == nil {
		return nil
	}
	mid := billing.MerchantID(job.Args.MerchantID)
	return w.DB.RunInMerchantConn(merchant.WithID(ctx, mid), func(ctx context.Context) error {
		return w.Disputes.Respond(ctx, job.Args)
	})
}
