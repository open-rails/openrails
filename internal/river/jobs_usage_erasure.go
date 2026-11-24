package riverjobs

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/usageerasure"
)

// UsageErasureWorker erases a customer's personal data from the event store.
// Without a store there is none.
type UsageErasureWorker struct {
	WorkerDefaults[usageerasure.Args]
	DB *db.DB
}

func (UsageErasureWorker) Kind() string { return usageerasure.Kind }

func (w UsageErasureWorker) Work(ctx context.Context, job *river.Job[usageerasure.Args]) error {
	store := w.DB.EventStore()
	if store == nil {
		return nil
	}
	return w.DB.RunInMerchantScope(ctx, billing.MerchantID(job.Args.MerchantID), "usage erasure", func(ctx context.Context) error {
		return w.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return usageerasure.Erase(ctx, gen.New(tx), store, job.Args.MerchantID, job.Args.CustomerID)
		})
	})
}
