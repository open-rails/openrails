package hosttools

import (
	"context"

	"github.com/jonboulle/clockwork"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/reconcile/converge"
)

// ConvergeMerchantOptions configures ConvergeMerchant.
type ConvergeMerchantOptions struct {
	Config     *config.Config
	PGXPool    *pgxpool.Pool
	MerchantID billing.MerchantID
	// Clock is the runtime's clock; nil reads wall time.
	Clock clockwork.Clock
}

// ConvergeMerchantResult summarizes one merchant-wide convergence pass.
type ConvergeMerchantResult struct {
	Findings          int
	AutoFixed         int
	ReconcileRequired int
	AdminRequired     int
}

// ConvergeMerchant runs one merchant-wide convergence pass on demand: the same
// idempotent engine as the scheduled sweep and the inline AfterMutation path,
// so after a legacy import it materializes every grant and entitlement without
// waiting for the sweep. A no-op when clean.
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
	engine := converge.NewConvergeEngine(database, opts.Clock)

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
