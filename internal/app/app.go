package app

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"

	vaultapi "github.com/hashicorp/vault/api"

	"github.com/open-rails/authkit/userinfo"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/billing"
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
	Log        *slog.Logger
}

// BootstrapOptions is what the application graph reaches outside its
// process: Logger and PGXPool are required; the rest are the host's optional
// dependencies and test seams.
type BootstrapOptions struct {
	// Logger receives everything the graph logs.
	Logger          *slog.Logger
	StripeTransport http.RoundTripper
	// BasisTheoryTransport substitutes the Basis Theory wire; test custody only.
	BasisTheoryTransport http.RoundTripper
	// NMITransport replaces the NMI wire at the real endpoints (test seam);
	// posture is still verified through it.
	NMITransport http.RoundTripper
	// FXTransport replaces the FX sources' wire (test seam).
	FXTransport     http.RoundTripper
	PGXPool         *pgxpool.Pool
	PostgresSession *pgxpool.Config
	Redis           redis.UniversalClient
	Clock           clockwork.Clock
	// EmailSender delivers OpenRails' email; nil sends none.
	EmailSender config.EmailSender
	// MessageRenderer is Deps.MessageRenderer.
	MessageRenderer func(context.Context, billing.MessageData) (billing.MessageContent, bool, error)
	// UserInfo is the host's directory (Deps.UserInfo); nil keeps a copy.
	UserInfo      userinfo.Lookup
	Notifications *config.NotificationChannel
	// Migrations replaces this build's migration files (test seam: an older
	// build's chain).
	Migrations fs.FS
	// Vault is the host's authenticated client (Deps.Vault).
	Vault *vaultapi.Client
}

// BootstrapWithOptions initialises core services with optional overrides.
// BootstrapWithOptions builds the application graph. ctx is the BOOT context:
// it bounds the wait for the database (xs-007 row 40 — a process asked to
// stop while its database is failing over stops; nothing else ends the wait).
func BootstrapWithOptions(ctx context.Context, cfg *config.Config, opts *BootstrapOptions) (*App, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config is required")
	}
	if opts == nil || opts.PGXPool == nil {
		return nil, fmt.Errorf("Deps.Postgres is required")
	}
	if opts.Logger == nil {
		return nil, fmt.Errorf("a logger is required")
	}
	logger := opts.Logger
	// Omission never disables the protective defaults; an empty RateLimits
	// does.
	if cfg.RateLimits == nil {
		cfg.RateLimits = config.DefaultRateLimits()
	}
	if cfg.Captcha == nil {
		cfg.Captcha = config.DefaultCaptcha()
	}
	// An optional setting that cannot be served is turned off and reported;
	// only what the engine cannot serve without fails.
	degraded := config.Degrade(cfg)
	if err := config.Validate(cfg); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	database, err := db.NewWithPGXPoolAndSessions(logger, opts.PGXPool, config.SchemaName(cfg), opts.PostgresSession)
	if err != nil {
		return nil, fmt.Errorf("use pgx pool: %w", err)
	}

	runtime, err := buildRuntimeWithOverrides(ctx, cfg, &runtimeOverrides{
		Logger:               logger,
		BasisTheoryTransport: opts.BasisTheoryTransport,
		StripeTransport:      opts.StripeTransport,
		NMITransport:         opts.NMITransport,
		FXTransport:          opts.FXTransport,
		DB:                   database,
		Vault:                opts.Vault,
		Redis:                opts.Redis,
		Clock:                opts.Clock,
		EmailSender:          opts.EmailSender,
		MessageRenderer:      opts.MessageRenderer,
		UserInfo:             opts.UserInfo,
		Notifications:        opts.Notifications,
		Migrations:           opts.Migrations,
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
		Log:         logger,
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
