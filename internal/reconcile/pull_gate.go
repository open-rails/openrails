package reconcile

import (
	"context"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/controls"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/destructive"
)

// OverwriteHeld is the merchant's gate on a pull's overwrite class, for the
// scheduled pull and the operator's alike. held says why local state may not
// be overwritten: the merchant's destructive actions are disarmed or its
// writes are readonly; empty when neither holds it. armed reports the
// destructive arming, which each caller weighs itself. ctx must be
// merchant-scoped.
func OverwriteHeld(ctx context.Context, database *db.DB, cfg *billing.Config, merchantID uuid.UUID) (held string, armed bool) {
	verdict := destructive.New(database).Check(ctx, merchantID)
	if !verdict.Allowed {
		return verdict.Reason, false
	}
	if posture := (controls.View{DB: database}).Posture(ctx, merchantID); posture.ReadOnly() {
		return posture.Reason, verdict.EnforceArmed
	}
	return "", verdict.EnforceArmed
}

// LockMerchantPull takes the merchant's pull lease: one mutating pull per
// merchant at a time, across every replica and the operator's CLI. It is nil
// while another pull holds it. The pull runs under its Context, which ends if
// the lease is lost; leases is the pool that renews it.
func LockMerchantPull(ctx context.Context, leases *db.DB, merchantID uuid.UUID) (*db.RunLease, error) {
	return leases.TryMerchantLease(ctx, merchantID, db.LeasePull, db.RunLeaseTTL)
}
