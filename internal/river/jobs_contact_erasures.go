package riverjobs

import (
	"context"
	"fmt"
	"time"

	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/scim"
)

// erasureRecheck is how soon a drain whose erasures still wait, on the
// budget or a hold, looks again; clearing the hold brings it forward.
const erasureRecheck = 10 * time.Minute

// ContactErasureWorker drains a merchant's waiting contact erasures within
// its erasure budget, and comes back while any wait.
type ContactErasureWorker struct {
	WorkerDefaults[destructive.DrainErasuresArgs]
	DB         *db.DB
	Admissions *destructive.Admissions
}

func (w *ContactErasureWorker) Work(ctx context.Context, job *river.Job[destructive.DrainErasuresArgs]) error {
	if w.DB == nil || w.Admissions == nil {
		return fmt.Errorf("contact erasure worker is not wired")
	}
	waiting, err := scim.DrainErasures(ctx, w.DB, w.Admissions, billing.MerchantID(job.Args.MerchantID))
	if err != nil {
		return err
	}
	if waiting {
		return river.JobSnooze(erasureRecheck)
	}
	return nil
}
