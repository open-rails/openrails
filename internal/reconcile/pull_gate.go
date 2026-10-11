package reconcile

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/writeposture"
)

// OverwriteHeld is the merchant's gate on a pull's overwrite class, for the
// scheduled pull and the operator's alike. held says why local state may not
// be overwritten: the destructive switch is off (instance or merchant) or the
// merchant's writes are readonly; empty when neither holds it. armed reports
// the first-enforce arming, which each caller weighs itself. ctx must be
// merchant-scoped.
func OverwriteHeld(ctx context.Context, database *db.DB, cfg *config.Config, merchantID uuid.UUID) (held string, armed bool) {
	verdict := destructive.New(database).Check(ctx, merchantID)
	if !verdict.Allowed {
		return verdict.Reason, false
	}
	if posture := (writeposture.View{Config: cfg, DB: database}).Posture(ctx, merchantID); posture.ReadOnly() {
		return posture.Reason, verdict.EnforceArmed
	}
	return "", verdict.EnforceArmed
}

// LockMerchantPull takes the merchant's pull lock: one mutating pull per
// merchant at a time, across every replica and the operator's CLI. held is
// false while another pull has it. The lock lives on its own connection until
// release.
func LockMerchantPull(ctx context.Context, pool *pgxpool.Pool, merchantID uuid.UUID) (release func(), held bool, err error) {
	if pool == nil {
		return nil, false, errors.New("pull lock: pool not configured")
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	q := gen.New(conn)
	if held, err = q.TryLockProviderRefresh(ctx, merchantID); err != nil || !held {
		conn.Release()
		return nil, false, err
	}
	return func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := q.UnlockProviderRefresh(unlockCtx, merchantID); err != nil {
			_ = conn.Conn().Close(unlockCtx)
		}
		conn.Release()
	}, true, nil
}
