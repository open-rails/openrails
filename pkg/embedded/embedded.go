package embedded

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"

	"github.com/redis/go-redis/v9"
	"github.com/uptrace/bun"

	"github.com/doujins-org/doujins-billing/config"
	"github.com/doujins-org/doujins-billing/internal/app"
	"github.com/doujins-org/doujins-billing/internal/server"
	"github.com/doujins-org/doujins-billing/pkg/authprovider"
	"github.com/doujins-org/doujins-billing/pkg/cache"
)

type Options struct {
	Config       *config.Config
	DB           *sql.DB
	BunDB        *bun.DB
	Redis        *redis.Client
	AuthProvider authprovider.Provider
	Cache        cache.Cache
}

type Embedded struct {
	app    *app.App
	server *server.Server
}

func New(opts Options) (*Embedded, error) {
	if opts.Config == nil {
		return nil, fmt.Errorf("config is required")
	}

	application, err := app.BootstrapWithOptions(opts.Config, &app.BootstrapOptions{
		DB:           opts.DB,
		BunDB:        opts.BunDB,
		Redis:        opts.Redis,
		AuthProvider: opts.AuthProvider,
		Cache:        opts.Cache,
	})
	if err != nil {
		return nil, fmt.Errorf("bootstrap application: %w", err)
	}
	cleanupOnError := true
	defer func() {
		if cleanupOnError {
			_ = application.Close(context.Background())
		}
	}()

	authProvider := application.AuthProvider
	if opts.AuthProvider != nil {
		authProvider = opts.AuthProvider
	}

	billingServer, err := server.New(server.Dependencies{
		Config:       application.Config,
		Cache:        application.Cache,
		Runtime:      application.Runtime,
		Redis:        application.RedisClient,
		AuthProvider: authProvider,
	})
	if err != nil {
		return nil, fmt.Errorf("create billing server: %w", err)
	}
	cleanupOnError = false

	return &Embedded{
		app:    application,
		server: billingServer,
	}, nil
}

func (e *Embedded) Handler() http.Handler {
	if e == nil || e.server == nil {
		return nil
	}
	return e.server.Handler()
}

func (e *Embedded) PrivateHandler() http.Handler {
	if e == nil || e.server == nil {
		return nil
	}
	return e.server.PrivateHandler()
}

func (e *Embedded) RunWorkers(ctx context.Context) error {
	if e == nil || e.app == nil || e.app.Runtime == nil {
		return fmt.Errorf("runtime is not initialized")
	}
	return e.app.Runtime.RunWorkers(ctx)
}

func (e *Embedded) Close(ctx context.Context) error {
	if e == nil {
		return nil
	}
	if e.server != nil {
		_ = e.server.Close(ctx)
	}
	if e.app != nil {
		return e.app.Close(ctx)
	}
	return nil
}

func (e *Embedded) Config() *config.Config {
	if e == nil || e.app == nil {
		return nil
	}
	return e.app.Config
}
