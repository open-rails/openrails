package app

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/auth"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/pkg/authprovider/ginauth"
	"github.com/open-rails/openrails/pkg/cache"
)

// App encapsulates the long-lived dependencies shared across transports.
type App struct {
	Config       *config.Config
	Runtime      *Runtime
	Cache        cache.Cache
	RedisClient  *redis.Client
	AuthProvider ginauth.Provider

	// ControlPlane is OpenRails' OpenRails-owned AuthKit control plane (#224).
	// nil when auth.control_plane is disabled (pure verifier mode).
	ControlPlane *controlplane.ControlPlane

	stopRedisMonitor context.CancelFunc
	// controlPlanePool is an OpenRails-owned pgx pool backing the control plane,
	// created only when the control plane is enabled and no pool was injected.
	controlPlanePool *pgxpool.Pool
}

// BootstrapOptions controls optional overrides for embedded use.
type BootstrapOptions struct {
	DB           *sql.DB
	PGXPool      *pgxpool.Pool
	Redis        *redis.Client
	AuthProvider ginauth.Provider
	Cache        cache.Cache
	Clock        clockwork.Clock
}

// Bootstrap initialises core services, caches, and auth verifier.
func Bootstrap(cfg *config.Config) (*App, error) {
	return BootstrapWithOptions(cfg, nil)
}

// BootstrapWithOptions initialises core services with optional overrides.
func BootstrapWithOptions(cfg *config.Config, opts *BootstrapOptions) (*App, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config is required")
	}
	if err := config.Validate(cfg); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	// Configure logger level
	if cfg.Logger != nil && cfg.Logger.Level != "" {
		level, err := log.ParseLevel(cfg.Logger.Level)
		if err != nil {
			log.WithError(err).Warnf("Invalid log level '%s', using default", cfg.Logger.Level)
		} else {
			log.SetLevel(level)
			log.Infof("Log level set to: %s", level)
		}
	}

	authProvider := ginauth.Provider(nil)
	if opts != nil && opts.AuthProvider != nil {
		authProvider = opts.AuthProvider
	} else {
		ap, err := auth.NewProvider(cfg.Auth)
		if err != nil {
			return nil, fmt.Errorf("build auth provider: %w", err)
		}
		authProvider = ap
	}

	var dbOverride *db.DB
	if opts != nil {
		switch {
		case opts.PGXPool != nil:
			if dbo, err := db.NewWithPGXPool(opts.PGXPool); err != nil {
				return nil, fmt.Errorf("use pgx pool: %w", err)
			} else {
				dbOverride = dbo
			}
		case opts.DB != nil:
			if dbo, err := db.NewWithSQLDB(opts.DB); err != nil {
				return nil, fmt.Errorf("use sql db: %w", err)
			} else {
				dbOverride = dbo
			}
		}
	}

	runtime, err := buildRuntimeWithOverrides(cfg, &runtimeOverrides{
		DB: dbOverride,
		Redis: func() *redis.Client {
			if opts != nil {
				return opts.Redis
			}
			return nil
		}(),
		Clock: func() clockwork.Clock {
			if opts != nil {
				return opts.Clock
			}
			return nil
		}(),
	})
	if err != nil {
		return nil, fmt.Errorf("initialise runtime: %w", err)
	}

	var appCache cache.Cache
	var stop context.CancelFunc
	if opts != nil && opts.Cache != nil {
		appCache = opts.Cache
	} else {
		memoryCache := cache.NewMemoryCache()
		switchable := cache.NewSwitchableCache(memoryCache)
		appCache = switchable
		if runtime.RedisClient != nil {
			stop = monitorRedis(runtime.RedisClient, switchable, memoryCache)
		} else {
			log.Warn("redis not configured; cache operating in-memory only")
		}
	}

	app := &App{
		Config:           cfg,
		Runtime:          runtime,
		Cache:            appCache,
		RedisClient:      runtime.RedisClient,
		AuthProvider:     authProvider,
		stopRedisMonitor: stop,
	}

	// Build the OpenRails-owned AuthKit control plane (#224) when enabled. This
	// is OFF by default; verifier-only deployments leave app.ControlPlane nil.
	if cfg.Auth != nil && cfg.Auth.ControlPlaneEnabled() {
		// The control plane needs a pgx pool over the database holding AuthKit's
		// profiles.* schema. Reuse an injected pool when provided, else create one.
		pool := func() *pgxpool.Pool {
			if opts != nil {
				return opts.PGXPool
			}
			return nil
		}()
		ownedPool := false
		if pool == nil {
			p, err := pgxpool.New(context.Background(), cfg.DB.GetConnectionString())
			if err != nil {
				return nil, fmt.Errorf("control plane: build pgx pool: %w", err)
			}
			pool = p
			ownedPool = true
		}
		cp, err := controlplane.New(context.Background(), cfg, pool)
		if err != nil {
			if ownedPool {
				pool.Close()
			}
			return nil, fmt.Errorf("build control plane: %w", err)
		}
		app.ControlPlane = cp
		if ownedPool {
			app.controlPlanePool = pool
		}
	}

	return app, nil
}

// RunControlPlaneBootstrap idempotently bootstraps the OpenRails-owned AuthKit
// control plane (#224): ensures the default tenant's operator org, OpenRails
// operator role, the openrails.* permission catalog, and an initial operator
// OAT. It is a no-op when the control plane is disabled.
//
// Call it AFTER migrations have run (so billing.tenants and profiles.* exist)
// and at startup. Safe to re-run.
func (a *App) RunControlPlaneBootstrap(ctx context.Context, opts controlplane.BootstrapOptions) (*controlplane.BootstrapResult, error) {
	if a == nil || a.ControlPlane == nil {
		return nil, nil
	}
	if !opts.MintInitialOAT {
		opts.MintInitialOAT = true
	}
	res, err := a.ControlPlane.Bootstrap(ctx, opts)
	if err != nil {
		return res, err
	}
	// #226: also ensure the managed-hosting platform superadmin org + role when
	// configured. No-op when no platform org is set (single-tenant / non-managed).
	if _, perr := a.ControlPlane.BootstrapPlatform(ctx); perr != nil {
		return res, fmt.Errorf("platform superadmin bootstrap: %w", perr)
	}
	return res, nil
}

// Close releases all resources owned by the application.
func (a *App) Close(ctx context.Context) error {
	if a == nil {
		return nil
	}
	if a.stopRedisMonitor != nil {
		a.stopRedisMonitor()
	}
	if a.controlPlanePool != nil {
		a.controlPlanePool.Close()
		a.controlPlanePool = nil
	}
	var errs []error
	if a.Cache != nil {
		if err := a.Cache.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close cache: %w", err))
		}
	}
	if a.Runtime != nil {
		if err := a.Runtime.Close(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("shutdown errors: %v", errs)
}

func monitorRedis(client *redis.Client, switchable *cache.SwitchableCache, fallback cache.Cache) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	redisCache := cache.NewRedisCache(client)

	// Initial probe
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	usingRedis := false
	if _, err := client.Ping(probeCtx).Result(); err == nil {
		switchable.SetBackend(redisCache)
		log.Info("redis available: using redis-backed cache")
		usingRedis = true
	} else {
		log.WithError(err).Warn("redis unavailable at startup; using in-memory cache")
	}
	probeCancel()

	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				pingCtx, pingCancel := context.WithTimeout(context.Background(), 2*time.Second)
				_, err := client.Ping(pingCtx).Result()
				pingCancel()
				if err == nil {
					if !usingRedis {
						switchable.SetBackend(redisCache)
						usingRedis = true
						log.Info("redis became available; switched cache backend")
					}
					continue
				}
				if usingRedis {
					switchable.SetBackend(fallback)
					usingRedis = false
					log.WithError(err).Warn("redis lost; reverting cache to memory")
				}
			}
		}
	}()

	return cancel
}
