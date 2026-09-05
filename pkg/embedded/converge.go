package embedded

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/reconcile/converge"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ConvergeMerchantOptions configures ConvergeMerchant.
type ConvergeMerchantOptions struct {
	Config     *config.Config
	PGXPool    *pgxpool.Pool
	MerchantID merchant.ID
}

// ConvergeMerchantResult summarizes one merchant-wide convergence pass.
type ConvergeMerchantResult struct {
	Findings          int
	AutoFixed         int
	ReconcileRequired int
	AdminRequired     int
}

// ConvergeMerchant runs ONE merchant-wide convergence pass (DERIVE → LIFE → CON,
// Customer=nil) on demand — the operator-triggerable "full reconcile" for the
// post-migration cutover (#637). It is the same idempotent engine the scheduled
// converge_sweep and the inline AfterMutation path use, so calling it right after
// a legacy migration materializes every derive-1 grant + entitlement immediately
// instead of waiting for the next sweep interval. Re-runnable / no-op when clean.
//
// Cutover note (#637): a FROM-SCRATCH migration (doujins #724, which stops writing
// entitlements) produces NO orphan entitlements, so this pass derives everything
// cleanly with grant provenance. An IN-PLACE cutover over data migrated by the OLD
// code must first revoke the migrate-written orphan entitlements (grant_id IS NULL)
// — otherwise derive-1's overlap-skip leaves them live-but-grant-less (access is
// preserved, provenance is not). Prefer re-migrating from scratch.
func ConvergeMerchant(ctx context.Context, opts ConvergeMerchantOptions) (ConvergeMerchantResult, error) {
	var res ConvergeMerchantResult
	if ctx == nil {
		ctx = context.Background()
	}
	database, err := openEmbeddedDB(ctx, opts.Config, opts.PGXPool)
	if err != nil {
		return res, err
	}
	defer database.Close()

	merchantID := opts.MerchantID
	if err := database.RequireMerchantID(ctx, merchantID); err != nil {
		return res, err
	}
	engine := converge.NewConvergeEngine(database)
	engine.Now = func() time.Time { return time.Now().UTC() }

	mctx := merchant.WithID(ctx, merchantID)
	err = database.RunInMerchantConn(mctx, func(ctx context.Context) error {
		out, e := engine.Converge(ctx, converge.Scope{Merchant: merchantID})
		if e != nil {
			return e
		}
		res = ConvergeMerchantResult{
			Findings:          out.Findings,
			AutoFixed:         out.AutoFixed,
			ReconcileRequired: out.ReconcileRequired,
			AdminRequired:     out.AdminRequired,
		}
		return nil
	})
	return res, err
}
