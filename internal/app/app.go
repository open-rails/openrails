package app

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"
	"github.com/uptrace/bun"

	"github.com/doujins-org/doujins-billing/config"
	"github.com/doujins-org/doujins-billing/internal/auth"
	"github.com/doujins-org/doujins-billing/internal/db"
	"github.com/doujins-org/doujins-billing/pkg/authprovider"
	"github.com/doujins-org/doujins-billing/pkg/cache"
)

// App encapsulates the long-lived dependencies shared across transports.
type App struct {
	Config       *config.Config
	Runtime      *Runtime
	Cache        cache.Cache
	RedisClient  *redis.Client
	AuthProvider authprovider.Provider

	stopRedisMonitor context.CancelFunc
}

// BootstrapOptions controls optional overrides for embedded use.
type BootstrapOptions struct {
	DB           *sql.DB
	BunDB        *bun.DB
	Redis        *redis.Client
	AuthProvider authprovider.Provider
	Cache        cache.Cache
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

	authProvider := authprovider.Provider(nil)
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
		case opts.BunDB != nil:
			if dbo, err := db.NewWithBun(opts.BunDB); err != nil {
				return nil, fmt.Errorf("use bun db: %w", err)
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

	return &App{
		Config:           cfg,
		Runtime:          runtime,
		Cache:            appCache,
		RedisClient:      runtime.RedisClient,
		AuthProvider:     authProvider,
		stopRedisMonitor: stop,
	}, nil
}

// Close releases all resources owned by the application.
func (a *App) Close(ctx context.Context) error {
	if a == nil {
		return nil
	}
	if a.stopRedisMonitor != nil {
		a.stopRedisMonitor()
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
