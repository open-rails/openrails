package riverjobs

import (
	"context"
	"fmt"

	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
)

// MerchantUsagePurgeWorker deletes a purged merchant's usage from the event
// store. The purge queued it with its commit; a failure is retried by River.
type MerchantUsagePurgeWorker struct {
	WorkerDefaults[merchants.UsagePurgeArgs]
	DB *db.DB
}

func (w MerchantUsagePurgeWorker) Work(ctx context.Context, job *river.Job[merchants.UsagePurgeArgs]) error {
	store := w.DB.EventStore()
	if store == nil {
		return fmt.Errorf("merchant %s's usage is in an event store this deployment has not attached", job.Args.MerchantID)
	}
	return store.DeleteMerchant(merchant.WithID(ctx, billing.MerchantID(job.Args.MerchantID)))
}
