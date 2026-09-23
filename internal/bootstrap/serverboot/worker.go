package serverboot

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	hostconfig "github.com/open-rails/openrails/hostauth/config"
	"github.com/open-rails/openrails/internal/app"
	embcp "github.com/open-rails/openrails/internal/operator"
	"github.com/open-rails/openrails/pkg/cache"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/redis/go-redis/v9"
)

// NewWorker contributes the same AuthKit jobs as a standalone API
// process. It constructs no listener and still leaves worker startup to its caller.
func NewWorker(ctx context.Context, cfg *config.Config, opts *Options) (*app.App, error) {
	application, err := app.BootstrapWithOptions(ctx, cfg, &app.BootstrapOptions{
		PGXPool:            optsValue(opts, func(o *Options) *pgxpool.Pool { return o.PGXPool }),
		Redis:              optsValue(opts, func(o *Options) *redis.Client { return o.Redis }),
		Cache:              optsValue(opts, func(o *Options) cache.Cache { return o.Cache }),
		Clock:              optsValue(opts, func(o *Options) clockwork.Clock { return o.Clock }),
		ConfiguredMerchant: optsValue(opts, func(o *Options) merchant.ID { return o.ConfiguredMerchant }),
	})
	if err != nil {
		return nil, err
	}
	if err := embcp.Attach(ctx, application, cfg, optsValue(opts, func(o *Options) *hostconfig.AuthConfig { return o.Auth }), optsValue(opts, func(o *Options) *pgxpool.Pool { return o.PGXPool })); err != nil {
		_ = application.Close(context.Background())
		return nil, fmt.Errorf("attach worker control plane: %w", err)
	}
	if err := ReconcileBootMerchantManifest(ctx, cfg, application,
		optsValue(opts, func(o *Options) string { return o.MerchantManifestPath }),
		optsValue(opts, func(o *Options) string { return o.NMIProbeV5BaseURL })); err != nil {
		_ = application.Close(context.Background())
		return nil, err
	}
	return application, nil
}
