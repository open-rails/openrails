package bootstrap

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	server "github.com/open-rails/openrails/internal/http"
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

// Result holds the application graph created by the composition root.
type Result struct {
	App    *app.App
	Server *server.Server
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

// NewServer constructs the application runtime and the HTTP server graph together.
func NewServer(cfg *config.Config, opts *Options) (*Result, error) {
	application, err := NewApp(cfg, opts)
	if err != nil {
		return nil, err
	}

	cleanupOnError := true
	defer func() {
		if cleanupOnError {
			_ = application.Close(context.Background())
		}
	}()

	billingServer, err := server.New(server.Dependencies{
		Config:        application.Config,
		Cache:         application.Cache,
		Runtime:       application.Runtime,
		Redis:         application.RedisClient,
		Authenticator: application.Authenticator,
		ControlPlane:  application.ControlPlane,
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
