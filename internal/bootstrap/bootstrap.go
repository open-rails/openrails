package bootstrap

import (
	"database/sql"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/cache"
)

// Options controls optional dependency overrides during application construction.
type Options struct {
	DB      *sql.DB
	PGXPool *pgxpool.Pool
	Redis   *redis.Client
	// Authenticator is the framework-neutral auth boundary (gin-free). When nil,
	// the default AuthKit-backed authenticator is built from config.
	Authenticator billingauth.Authenticator
	Cache         cache.Cache
	Clock         clockwork.Clock
}

// NewApp constructs the long-lived application runtime.
func NewApp(cfg *config.Config, opts *Options) (*app.App, error) {
	application, err := app.BootstrapWithOptions(cfg, &app.BootstrapOptions{
		DB:            optsValue(opts, func(o *Options) *sql.DB { return o.DB }),
		PGXPool:       optsValue(opts, func(o *Options) *pgxpool.Pool { return o.PGXPool }),
		Redis:         optsValue(opts, func(o *Options) *redis.Client { return o.Redis }),
		Authenticator: optsValue(opts, func(o *Options) billingauth.Authenticator { return o.Authenticator }),
		Cache:         optsValue(opts, func(o *Options) cache.Cache { return o.Cache }),
		Clock:         optsValue(opts, func(o *Options) clockwork.Clock { return o.Clock }),
	})
	if err != nil {
		return nil, fmt.Errorf("bootstrap application: %w", err)
	}
	return application, nil
}

func optsValue[T any](opts *Options, pick func(*Options) T) T {
	var zero T
	if opts == nil {
		return zero
	}
	return pick(opts)
}
