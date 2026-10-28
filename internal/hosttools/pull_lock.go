package hosttools

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/reconcile"
)

// pullLockPoll is how often a waiting prune asks for the merchant's lock again.
const pullLockPoll = 2 * time.Second

// waitForMerchantPullLock takes the lock the scheduled pull holds, waiting
// while another pull of the merchant runs, so a prune never runs beside one.
func waitForMerchantPullLock(ctx context.Context, pool *pgxpool.Pool, merchantID billing.MerchantID) (release func(), err error) {
	for waited := false; ; waited = true {
		release, held, err := reconcile.LockMerchantPull(ctx, pool, merchantID.UUID())
		if err != nil {
			return nil, fmt.Errorf("merchant pull lock: %w", err)
		}
		if held {
			return release, nil
		}
		if !waited {
			log.WithContext(ctx).WithField("merchant_id", merchantID.String()).Info("another pull of this merchant is running; waiting for it to finish")
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for the merchant's pull lock: %w", ctx.Err())
		case <-time.After(pullLockPoll):
		}
	}
}
