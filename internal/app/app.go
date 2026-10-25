package app

import (
	"context"
	"fmt"
	vaultapi "github.com/hashicorp/vault/api"
	"io/fs"
	"net/http"

	"github.com/open-rails/authkit/userinfo"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
)

// App encapsulates the long-lived dependencies shared across transports.
type App struct {
	Config      *config.Config
	Runtime     *Runtime
	RedisClient redis.UniversalClient

	// Standalone marks the engine of a standalone server (package server),
	// which owns merchant identity and the HTTP surface: the embedded mount
	// refuses.
	Standalone bool
}

// BootstrapOptions controls optional overrides for embedded use. Embedded
// hosts supply their database as a pgx pool (PGXPool); the bun-era *sql.DB
// override was removed with the ORM (#334).
type BootstrapOptions struct {
	StripeTransport http.RoundTripper
	// CustodianCredentialTransport substitutes read-only vendor account verification; test claims only.
	CustodianCredentialTransport http.RoundTripper
	// NMITransport replaces the NMI wire at the real endpoints (test seam);
	// posture is still verified through it.
	NMITransport http.RoundTripper
	// FXTransport replaces the FX sources' wire (test seam).
	FXTransport http.RoundTripper
	PGXPool     *pgxpool.Pool
	Redis       redis.UniversalClient
	Clock       clockwork.Clock
	// EmailSender replaces the sender Config.SMTP selects.
	EmailSender config.EmailSender
	// UserInfo is the host's directory (Deps.UserInfo); nil keeps a copy.
	UserInfo      userinfo.Lookup
	Notifications *config.NotificationChannel
	// Migrations replaces this build's migration files (test seam: an older
	// build's chain).
	Migrations fs.FS
	// Vault is the host's authenticated client (Deps.Vault); nil logs in from
	// Config.Vault.
	Vault *vaultapi.Client
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
	// An optional setting that cannot be served is turned off and reported;
	// only what the engine cannot serve without fails.
	degraded := config.Degrade(cfg)
	degraded = append(degraded, config.DegradeEmail(cfg, opts != nil && opts.EmailSender != nil)...)
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
		CustodianCredentialTransport: func() http.RoundTripper {
			if opts != nil {
				return opts.CustodianCredentialTransport
			}
			return nil
		}(),
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
		FXTransport: func() http.RoundTripper {
			if opts != nil {
				return opts.FXTransport
			}
			return nil
		}(),
		DB: dbOverride,
		Vault: func() *vaultapi.Client {
			if opts != nil {
				return opts.Vault
			}
			return nil
		}(),
		Redis: func() redis.UniversalClient {
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
		EmailSender: func() config.EmailSender {
			if opts != nil {
				return opts.EmailSender
			}
			return nil
		}(),
		UserInfo: func() userinfo.Lookup {
			if opts != nil {
				return opts.UserInfo
			}
			return nil
		}(),
		Notifications: func() *config.NotificationChannel {
			if opts != nil {
				return opts.Notifications
			}
			return nil
		}(),
		Migrations: func() fs.FS {
			if opts != nil {
				return opts.Migrations
			}
			return nil
		}(),
	})
	if err != nil {
		return nil, fmt.Errorf("initialise runtime: %w", err)
	}
	runtime.RefuseDegraded(degraded)
	runtime.startRedisMonitor()

	app := &App{
		Config:      cfg,
		Runtime:     runtime,
		RedisClient: runtime.RedisClient,
	}

	return app, nil
}

// Close releases all resources owned by the application.
func (a *App) Close(ctx context.Context) error {
	if a == nil {
		return nil
	}
	var errs []error
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
