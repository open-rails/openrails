package app

import (
	"context"
	"fmt"
	"github.com/open-rails/openrails/internal/identity"
	"io/fs"
	"net"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
)

// App encapsulates the long-lived dependencies shared across transports.
type App struct {
	Config      *config.Config
	Runtime     *Runtime
	RedisClient *redis.Client

	// ControlPlane is an optional host-owned lifecycle resource. Concrete
	// identity capabilities are supplied by standalone composition; the billing
	// runtime only owns its cleanup.
	ControlPlane interface{ Close() }
	// ConsoleAssets is the admin console SPA (#754, web/admin), served by the
	// standalone surface when admin_console is enabled.
	ConsoleAssets fs.FS

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
	// DNSResolver answers api_host proof lookups (test seam).
	DNSResolver *net.Resolver
	PGXPool     *pgxpool.Pool
	Redis       *redis.Client
	Clock       clockwork.Clock
	// UserDirectory and UsernameResolver are explicit host identity seams.
	// OpenRails never assumes ownership of AuthKit's profiles schema.
	UserDirectory    identity.UserDirectory
	UsernameResolver identity.UsernameResolver
	// EmailSender replaces the sender Config.SendGrid selects.
	EmailSender config.EmailSender

	ConfiguredMerchant billing.MerchantID
}

// Bootstrap initialises core services and the auth verifier.
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
		if cfg.RateLimits == nil {
			cfg.RateLimits = config.DefaultRateLimits()
		}
		if cfg.Captcha == nil {
			cfg.Captcha = config.DefaultCaptcha()
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
		dbo, err := db.NewWithPGXPool(opts.PGXPool, config.SchemaName(cfg))
		if err != nil {
			return nil, fmt.Errorf("use pgx pool: %w", err)
		}
		dbOverride = dbo
	}

	runtime, err := buildRuntimeWithOverrides(ctx, cfg, &runtimeOverrides{
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
		DNSResolver: func() *net.Resolver {
			if opts != nil {
				return opts.DNSResolver
			}
			return nil
		}(),
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
		UserDirectory: func() identity.UserDirectory {
			if opts != nil {
				return opts.UserDirectory
			}
			return nil
		}(),
		UsernameResolver: func() identity.UsernameResolver {
			if opts != nil {
				return opts.UsernameResolver
			}
			return nil
		}(),
		EmailSender: func() config.EmailSender {
			if opts != nil {
				return opts.EmailSender
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

	runtime.startRedisMonitor()

	app := &App{
		Config:      cfg,
		Runtime:     runtime,
		RedisClient: runtime.RedisClient,
	}

	// The OpenRails-owned AuthKit control plane (#224) is no longer built here
	// (#284): the core stays AuthKit-free. The standalone/opt-in path builds it and
	// attaches via SetControlPlane (see internal/operator.AttachWithOptions).

	return app, nil
}

// Close releases all resources owned by the application.
func (a *App) Close(ctx context.Context) error {
	if a == nil {
		return nil
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
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("shutdown errors: %v", errs)
}
