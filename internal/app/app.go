package app

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/pkg/cache"
	"github.com/open-rails/openrails/pkg/merchant"
)

// App encapsulates the long-lived dependencies shared across transports.
type App struct {
	Config      *config.Config
	Runtime     *Runtime
	Cache       cache.Cache
	RedisClient *redis.Client

	// ControlPlane is OpenRails' OpenRails-owned AuthKit control plane (#224),
	// held as `any` so the embedded CORE (internal/app, pkg/embedded, embedhttp)
	// imports neither internal/controlplane nor — through it — AuthKit (#284).
	// It is nil only for embedded hosts that never attach one; the standalone
	// path ALWAYS attaches the concrete *controlplane.ControlPlane via
	// SetControlPlane (#469) and recovers it with a type assertion (see
	// pkg/embedded/controlplane).
	ControlPlane any

	stopRedisMonitor context.CancelFunc
	// controlPlanePool is an OpenRails-owned pgx pool backing the control plane,
	// created only when the control plane is attached and no pool was injected. It
	// is attached together with ControlPlane via SetControlPlane and closed here.
	controlPlanePool *pgxpool.Pool
}

// SetControlPlane attaches the OpenRails-owned AuthKit control plane built by the
// opt-in/standalone path (#284). cp is the concrete *controlplane.ControlPlane
// (kept as `any` here so the core stays AuthKit-free); ownedPool, when non-nil,
// is an OpenRails-owned pgx pool whose lifecycle App.Close manages.
func (a *App) SetControlPlane(cp any, ownedPool *pgxpool.Pool) {
	if a == nil {
		return
	}
	a.ControlPlane = cp
	if ownedPool != nil {
		a.controlPlanePool = ownedPool
	}
}

// BootstrapOptions controls optional overrides for embedded use. Embedded
// hosts supply their database as a pgx pool (PGXPool); the bun-era *sql.DB
// override was removed with the ORM (#334).
type BootstrapOptions struct {
	PGXPool *pgxpool.Pool
	Redis   *redis.Client
	Cache   cache.Cache
	Clock   clockwork.Clock

	ConfiguredMerchant merchant.ID
	Rails              config.RailMerchantAccountSet
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

	var dbOverride *db.DB
	if opts != nil && opts.PGXPool != nil {
		dbo, err := db.NewWithPGXPool(opts.PGXPool, cfg.DB.SchemaName())
		if err != nil {
			return nil, fmt.Errorf("use pgx pool: %w", err)
		}
		dbOverride = dbo
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
		Rails: func() config.RailMerchantAccountSet {
			if opts != nil {
				return opts.Rails
			}
			return nil
		}(),
	})
	if err != nil {
		return nil, fmt.Errorf("initialise runtime: %w", err)
	}
	if opts != nil {
		runtime.SetConfiguredMerchant(opts.ConfiguredMerchant)
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
		stopRedisMonitor: stop,
	}

	// The OpenRails-owned AuthKit control plane (#224) is no longer built here
	// (#284): the core stays AuthKit-free. The standalone/opt-in path builds it and
	// attaches via SetControlPlane (see pkg/embedded/controlplane.Attach).

	return app, nil
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
