package bootstrap

import (
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/cache"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Options controls optional dependency overrides during application construction.
type Options struct {
	PGXPool *pgxpool.Pool
	Redis   *redis.Client
	// Authenticator is the framework-neutral auth boundary (gin-free). When nil,
	// the default AuthKit-backed authenticator is built from config.
	Authenticator billingauth.Authenticator
	// DelegatedAuthenticator is the optional host-pluggable identity seam for
	// the delegated self-service surface (#339). When nil, that surface
	// authenticates via the control plane's delegated-token verifier.
	DelegatedAuthenticator billingauth.DelegatedAuthenticator
	Cache                  cache.Cache
	Clock                  clockwork.Clock
	ConfiguredMerchant     merchant.ID
	Rails                  config.RailSet
	CredentialMode         app.CredentialMode
}

// NewApp constructs the long-lived application runtime.
func NewApp(cfg *config.Config, opts *Options) (*app.App, error) {
	application, err := app.BootstrapWithOptions(cfg, &app.BootstrapOptions{
		PGXPool:                optsValue(opts, func(o *Options) *pgxpool.Pool { return o.PGXPool }),
		Redis:                  optsValue(opts, func(o *Options) *redis.Client { return o.Redis }),
		Authenticator:          optsValue(opts, func(o *Options) billingauth.Authenticator { return o.Authenticator }),
		DelegatedAuthenticator: optsValue(opts, func(o *Options) billingauth.DelegatedAuthenticator { return o.DelegatedAuthenticator }),
		Cache:                  optsValue(opts, func(o *Options) cache.Cache { return o.Cache }),
		Clock:                  optsValue(opts, func(o *Options) clockwork.Clock { return o.Clock }),
		ConfiguredMerchant:     optsValue(opts, func(o *Options) merchant.ID { return o.ConfiguredMerchant }),
		Rails:                  optsValue(opts, func(o *Options) config.RailSet { return o.Rails }),
		CredentialMode:         optsValue(opts, func(o *Options) app.CredentialMode { return o.CredentialMode }),
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
