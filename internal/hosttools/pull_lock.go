package hosttools

import (
	"context"
	"fmt"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/reconcile"
)

// pullLockPoll is how often a waiting prune asks for the merchant's lease again.
const pullLockPoll = 2 * time.Second

// waitForMerchantPullLock takes the lease the scheduled pull holds, waiting
// while another pull of the merchant runs, so a prune never runs beside one.
func waitForMerchantPullLock(ctx context.Context, database *db.DB, merchantID billing.MerchantID) (*db.RunLease, error) {
	for waited := false; ; waited = true {
		lease, err := reconcile.LockMerchantPull(ctx, database, merchantID.UUID())
		if err != nil {
			return nil, fmt.Errorf("merchant pull lock: %w", err)
		}
		if lease != nil {
			return lease, nil
		}
		if !waited {
			database.Log.InfoContext(ctx, "another pull of this merchant is running; waiting for it to finish", "merchant_id", merchantID.String())
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for the merchant's pull lock: %w", ctx.Err())
		case <-time.After(pullLockPoll):
		}
	}
}
