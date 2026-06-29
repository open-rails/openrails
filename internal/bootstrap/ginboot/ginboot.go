// Package ginboot holds the gin-coupled composition root (#285): NewServer wires
// the gin HTTP Server onto the gin-free application graph built by
// bootstrap.NewApp. Keeping it out of internal/bootstrap is what lets the
// gin-free composition root (and pkg/embedded through it) avoid importing gin.
package ginboot

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	internalauth "github.com/open-rails/openrails/internal/auth"
	"github.com/open-rails/openrails/internal/bootstrap"
	server "github.com/open-rails/openrails/internal/http"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/cache"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Result holds the application graph plus the gin HTTP server created by the
// composition root.
type Result struct {
	App    *app.App
	Server *server.Server
}

// Options controls optional dependency overrides for the standalone gin server
// composition root.
type Options struct {
	PGXPool *pgxpool.Pool
	Redis   *redis.Client
	Cache   cache.Cache
	Clock   clockwork.Clock

	// Authenticator protects user routes. When nil, standalone uses the attached
	// control plane verifier.
	Authenticator billingauth.Authenticator
	// DelegatedAuthenticator is the optional host-pluggable identity seam for
	// self-service routes. When nil, those routes use control-plane delegation.
	DelegatedAuthenticator billingauth.DelegatedAuthenticator

	ConfiguredMerchant merchant.ID
	Rails              config.ProviderAccountSet
}

// NewServer constructs the application runtime and the gin HTTP server graph
// together. It is the gin-coupled counterpart of bootstrap.NewApp.
func NewServer(cfg *config.Config, opts *Options) (*Result, error) {
	application, err := bootstrap.NewApp(cfg, &bootstrap.Options{
		PGXPool:            optsValue(opts, func(o *Options) *pgxpool.Pool { return o.PGXPool }),
		Redis:              optsValue(opts, func(o *Options) *redis.Client { return o.Redis }),
		Cache:              optsValue(opts, func(o *Options) cache.Cache { return o.Cache }),
		Clock:              optsValue(opts, func(o *Options) clockwork.Clock { return o.Clock }),
		ConfiguredMerchant: optsValue(opts, func(o *Options) merchant.ID { return o.ConfiguredMerchant }),
		Rails:              optsValue(opts, func(o *Options) config.ProviderAccountSet { return o.Rails }),
	})
	if err != nil {
		return nil, err
	}

	cleanupOnError := true
	defer func() {
		if cleanupOnError {
			_ = application.Close(context.Background())
		}
	}()

	// Standalone always attaches the OpenRails-owned AuthKit control plane
	// (#284/#469), reusing an injected pool when present. Failure is fatal.
	var injectedPool = func() *pgxpool.Pool {
		if opts != nil {
			return opts.PGXPool
		}
		return nil
	}()
	if cperr := embcp.Attach(context.Background(), application, cfg, injectedPool); cperr != nil {
		return nil, fmt.Errorf("attach control plane: %w", cperr)
	}
	authenticator := optsValue(opts, func(o *Options) billingauth.Authenticator { return o.Authenticator })
	if authenticator == nil {
		cp := embcp.Get(application)
		if cp == nil || cp.AuthService() == nil || cp.AuthService().Verifier() == nil {
			return nil, fmt.Errorf("control plane verifier unavailable")
		}
		authenticator = internalauth.NewAuthenticator(cp.AuthService().Verifier())
	}

	billingServer, err := server.New(server.Dependencies{
		Config:                 application.Config,
		Cache:                  application.Cache,
		Runtime:                application.Runtime,
		Redis:                  application.RedisClient,
		Authenticator:          authenticator,
		DelegatedAuthenticator: optsValue(opts, func(o *Options) billingauth.DelegatedAuthenticator { return o.DelegatedAuthenticator }),
		ConfiguredMerchant:     application.Runtime.ConfiguredMerchant,
		ControlPlane:           embcp.Get(application),
	})
	if err != nil {
		return nil, fmt.Errorf("create billing server: %w", err)
	}

	cleanupOnError = false
	return &Result{App: application, Server: billingServer}, nil
}

func optsValue[T any](opts *Options, pick func(*Options) T) T {
	var zero T
	if opts == nil {
		return zero
	}
	return pick(opts)
}
