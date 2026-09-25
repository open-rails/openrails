package app

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/retry"
	"github.com/open-rails/openrails/pkg/cache"
	"github.com/open-rails/openrails/pkg/merchant"
)

// App encapsulates the long-lived dependencies shared across transports.
type App struct {
	Config      *config.Config
	Runtime     *Runtime
	Cache       cache.Cache
	RedisClient *redis.Client

	// ControlPlane is an optional host-owned lifecycle resource. Concrete
	// identity capabilities are supplied by standalone composition; the billing
	// runtime only owns its cleanup.
	ControlPlane interface{ Close() }
	// ConsoleAssets is the host-built admin console SPA (#754), served by the
	// standalone surface when admin_console is enabled.
	ConsoleAssets fs.FS

	stopRedisMonitor context.CancelFunc
	ownedCache       cache.Cache
	// controlPlanePool is an OpenRails-owned pgx pool backing the control plane,
	// created only when the control plane is attached and no pool was injected. It
	// is attached together with ControlPlane via SetControlPlane and closed here.
	controlPlanePool *pgxpool.Pool
}

// SetControlPlane registers host identity cleanup and its optional owned pool.
// Borrowed pools remain owned by the host.
func (a *App) SetControlPlane(cp interface{ Close() }, ownedPool *pgxpool.Pool) {
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
	StripeTransport http.RoundTripper
	// NMITransport replaces the NMI wire at the real endpoints (test seam);
	// posture is still verified through it.
	NMITransport http.RoundTripper
	HostRiver    bool
	RiverSchema  string
	PGXPool      *pgxpool.Pool
	Redis        *redis.Client
	Cache        cache.Cache
	Clock        clockwork.Clock
	// UserDirectory and UsernameResolver are explicit host identity seams.
	// OpenRails never assumes ownership of AuthKit's profiles schema.
	UserDirectory    openrails.UserDirectory
	UsernameResolver openrails.UsernameResolver

	ConfiguredMerchant merchant.ID
}

// Bootstrap initialises core services, caches, and auth verifier.
func Bootstrap(ctx context.Context, cfg *config.Config) (*App, error) {
	return BootstrapWithOptions(ctx, cfg, nil)
}

// BootstrapWithOptions initialises core services with optional overrides.
// BootstrapWithOptions builds the application graph. ctx is the BOOT context:
// it bounds the wait for the database (xs-007 row 40 — a process asked to
// stop while its database is failing over stops; nothing else ends the wait).
func BootstrapWithOptions(ctx context.Context, cfg *config.Config, opts *BootstrapOptions) (*App, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config is required")
	}
	// Programmatic standalone construction needs the same protective defaults
	// as the file loader and embedded constructor. Omission never disables them.
	if !cfg.RateLimitsDisabled {
		defaults := config.GetDefaultBillingConfig()
		if cfg.RateLimits == nil {
			cfg.RateLimits = defaults.RateLimits
		}
		if cfg.Captcha == nil {
			cfg.Captcha = defaults.Captcha
		}
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

	runtime, err := buildRuntimeWithOverrides(ctx, cfg, &runtimeOverrides{
		HostRiver: opts != nil && opts.HostRiver,
		StripeTransport: func() http.RoundTripper {
			if opts != nil {
				return opts.StripeTransport
			}
			return nil
		}(),
		NMITransport: func() http.RoundTripper {
			if opts != nil {
				return opts.NMITransport
			}
			return nil
		}(),
		DB: dbOverride,
		RiverSchema: func() string {
			if opts != nil {
				return opts.RiverSchema
			}
			return ""
		}(),
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
		UserDirectory: func() openrails.UserDirectory {
			if opts != nil {
				return opts.UserDirectory
			}
			return nil
		}(),
		UsernameResolver: func() openrails.UsernameResolver {
			if opts != nil {
				return opts.UsernameResolver
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
	var ownedCache cache.Cache
	var stop context.CancelFunc
	if opts != nil && opts.Cache != nil {
		appCache = opts.Cache
	} else {
		memoryCache := cache.NewMemoryCache()
		ownedCache = memoryCache
		switchable := cache.NewSwitchableCache(memoryCache)
		appCache = switchable
		if runtime.RedisClient != nil {
			stop = monitorRedis(runtime.RedisClient, switchable, memoryCache, runtime.redisState.record)
		} else {
			log.Warn("redis not configured; cache operating in-memory only")
		}
	}

	app := &App{
		Config:           cfg,
		Runtime:          runtime,
		Cache:            appCache,
		ownedCache:       ownedCache,
		RedisClient:      runtime.RedisClient,
		stopRedisMonitor: stop,
	}

	// The OpenRails-owned AuthKit control plane (#224) is no longer built here
	// (#284): the core stays AuthKit-free. The standalone/opt-in path builds it and
	// attaches via SetControlPlane (see embed/controlplane.Attach).

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
	var errs []error
	// Shared workers must stop before their optional components release pools.
	if a.Runtime != nil {
		if err := a.Runtime.Close(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if a.ControlPlane != nil {
		a.ControlPlane.Close()
	}
	a.ControlPlane = nil
	if a.controlPlanePool != nil {
		a.controlPlanePool.Close()
		a.controlPlanePool = nil
	}
	// Close the owned fallback even if Redis is the current backend.
	// A cache supplied by the host stays open.
	if a.ownedCache != nil {
		if err := a.ownedCache.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close cache: %w", err))
		}
		a.ownedCache = nil
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("shutdown errors: %v", errs)
}

// monitorRedis starts on the memory cache and switches to Redis once a probe
// answers, back to memory when one fails: every 10s while up, capped
// full-jitter backoff while down. Construction never waits on Redis.
func monitorRedis(client *redis.Client, switchable *cache.SwitchableCache, fallback cache.Cache, record func(error)) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	redisCache := cache.NewRedisCache(client)
	go func() {
		usingRedis := false
		for attempt := 0; ; {
			pingCtx, pingCancel := context.WithTimeout(ctx, 2*time.Second)
			err := client.Ping(pingCtx).Err()
			pingCancel()
			if ctx.Err() != nil {
				return
			}
			record(err)
			wait := 10 * time.Second
			switch {
			case err == nil && !usingRedis:
				switchable.SetBackend(redisCache)
				usingRedis = true
				log.Info("redis available; cache uses redis")
			case err != nil && usingRedis:
				switchable.SetBackend(fallback)
				usingRedis = false
				log.WithError(err).Warn("redis lost; cache reverted to memory")
			}
			if err != nil {
				wait = retry.Backoff(attempt, retry.Base, retry.Max)
				attempt++
			} else {
				attempt = 0
			}
			if !retry.Sleep(ctx, wait) {
				return
			}
		}
	}()
	return cancel
}

// HostGraph returns the application graph behind a public runtime handle
// (*embed.Runtime). Package embed registers it at init so operator packages
// reach the graph without the runtime exporting internal types.
var HostGraph func(runtime any) *App
