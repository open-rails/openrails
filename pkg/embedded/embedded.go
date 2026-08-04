package embedded

import (
	"context"
	"fmt"
	"io/fs"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/pkg/cache"
	"github.com/open-rails/openrails/pkg/service"
)

// RouteSet names a mountable billing HTTP route group.
type RouteSet = embedhttp.RouteSet

const (
	RouteSetCheckout         = embedhttp.RouteSetCheckout
	RouteSetCustomer         = embedhttp.RouteSetCustomer
	RouteSetMerchantAdmin    = embedhttp.RouteSetMerchantAdmin
	RouteSetCatalog          = embedhttp.RouteSetCatalog
	RouteSetPaymentProviders = embedhttp.RouteSetPaymentProviders
	RouteSetMerchantAPI      = embedhttp.RouteSetMerchantAPI
	RouteSetWebhooks         = embedhttp.RouteSetWebhooks
)

var (
	EmbeddedDefaultRouteSets   = append([]RouteSet(nil), embedhttp.EmbeddedDefaultRouteSets...)
	StandaloneDefaultRouteSets = append([]RouteSet(nil), embedhttp.StandaloneDefaultRouteSets...)
)

type Options struct {
	// Config is built programmatically by the host — embedded mode never runs
	// config.Load, so none of Load's defaulting applies. New() refuses to
	// construct unless the host has declared its posture explicitly (#745):
	//   - Config.Env must be non-empty (no implicit dev-like "" posture).
	//   - Config.TestMode must be config.CredentialPostureSandbox or
	//     config.CredentialPostureLive (the zero value is UNSET, never "live"
	//     by default — supersedes #711's warn-only).
	// New() also seeds Config.RateLimits/Config.Captcha with the same curated
	// defaults config.Load applies whenever the host leaves them nil (#742),
	// unless Config.RateLimitsDisabled opts out.
	Config *config.Config
	// PGXPool is the host-supplied database handle (pgx/v5). The bun-era
	// *sql.DB option was removed with the ORM (#334).
	PGXPool *pgxpool.Pool
	Redis   *redis.Client
	Cache   cache.Cache
	// ConsoleAssets is the HOST-BUILT admin console SPA (#754: go:embed cannot
	// cross module boundaries, so whoever builds the binary owns the embed and
	// hands the engine an fs.FS rooted at index.html). nil = no frontend bytes;
	// admin_console.enabled without assets refuses to build the standalone
	// surface. Hosts using embed.New pass embed.WithAdminConsole instead.
	ConsoleAssets fs.FS
	// River declares who owns the River job fleet. REQUIRED (#895): OpenRails'
	// periodic fleet is where the money moves, so construction refuses rather
	// than returning an engine whose money jobs silently never run. Use
	// RiverFromHost(bind) when the host owns River (the embedded posture), or
	// RiverManagedByOpenRails() to have OpenRails construct and run its own
	// (standalone). The zero value is the rejected "nobody" state.
	River RiverOwnership
}

type Embedded struct {
	app *app.App
	// consoleAssets is Options.ConsoleAssets, consumed by StandaloneServer.
	consoleAssets fs.FS
	// activeRouteSets records the resolved route groups of the most recently
	// mounted HTTP surface (#623), for ActiveRouteSets() and capability
	// discovery. Written at mount time (boot), read while serving.
	activeRouteSets []RouteSet
}

func New(opts Options) (*Embedded, error) {
	if opts.Config == nil {
		return nil, fmt.Errorf("config is required")
	}
	// #895: refuse BEFORE building the application graph — an engine with no
	// declared River owner is not a usable engine, and returning one is what
	// made "the host forgot River entirely" indistinguishable from a healthy
	// boot.
	if !opts.River.declared() {
		return nil, ErrRiverRequired
	}
	if err := applyEmbeddedDefaults(opts.Config); err != nil {
		return nil, err
	}
	// Build the application graph only; HTTP surfaces (StandaloneHandler /
	// MountHandler) are constructed from this App on demand. (#711: the
	// bootstrap.Options relay layer is gone — this calls the app composition
	// root directly.) Rail credentials are NEVER boot options (#788): rails
	// arm per merchant through the manifest (MODE 1) or the management API
	// (MODE 2) into psps + the merchant secret store.
	application, err := app.BootstrapWithOptions(opts.Config, &app.BootstrapOptions{
		PGXPool: opts.PGXPool,
		Redis:   opts.Redis,
		Cache:   opts.Cache,
	})
	if err != nil {
		return nil, fmt.Errorf("bootstrap application: %w", err)
	}

	e := &Embedded{app: application, consoleAssets: opts.ConsoleAssets}
	ctx := context.Background()
	if err := e.bindRiver(ctx, opts.River); err != nil {
		_ = application.Close(ctx)
		return nil, err
	}
	// #895: start the out-of-River progress detector. It is deliberately started
	// at CONSTRUCTION, not in RunWorkers — a host that never calls RunWorkers is
	// exactly the case that used to be undetectable.
	application.Runtime.StartRiverProgressMonitor(ctx)
	return e, nil
}

// applyEmbeddedDefaults enforces the posture embedded construction must
// declare explicitly (#745) and seeds the protective defaults config.Load
// applies (#742) — both because embedded hosts build Config programmatically
// and never run Load's defaulting/validation pipeline. Mutates cfg in place;
// called once, early, before the application graph is built.
func applyEmbeddedDefaults(cfg *config.Config) error {
	// #745: an empty Env reads as "development" throughout this package
	// (IsDev/RequiresRLS/RequiresSecretEncryption/Validate's isDev gate),
	// disabling every hard-gate check below. Standalone Load() defaults dev
	// boots to sandbox-safe behavior on top of that; embedded construction has
	// no such safety net, so it must never inherit the dev-like default by
	// omission — the host must say which environment this is.
	if strings.TrimSpace(cfg.Env) == "" {
		return fmt.Errorf("embedded: config.Env is required (#745) — embedded construction never runs config.Load's dev-like empty-Env default; set it explicitly (e.g. \"development\", \"staging\", \"production\")")
	}
	// #745: the zero value of the old bool TestMode field silently meant LIVE
	// credentials; CredentialPosture keeps that trap from recurring by making
	// "unset" a third, rejected state instead of overlapping with "live"
	// (supersedes #711's warn-only). Standalone Load() resolves this
	// explicitly (dev defaults to sandbox, everything else to live) before
	// Validate ever runs; embedded construction has no such step.
	switch cfg.TestMode {
	case config.CredentialPostureSandbox, config.CredentialPostureLive:
	default:
		return fmt.Errorf("embedded: config.TestMode is required (#745) — set config.CredentialPostureSandbox or config.CredentialPostureLive explicitly; embedded construction never runs config.Load's dev-defaults-to-sandbox fallback, so an unset value must never be assumed")
	}

	// #742: RateLimitHTTP(nil, ...) is a full passthrough — a host that never
	// set RateLimits ships completely unthrottled. Seed the same curated
	// defaults config.Load applies, unless the host explicitly opted out (a
	// host that fronts billing with its own gateway/rate limiter). A host that
	// already set its own RateLimits/Captcha is left alone either way.
	if !cfg.RateLimitsDisabled {
		defaults := config.GetDefaultBillingConfig()
		if cfg.RateLimits == nil {
			cfg.RateLimits = defaults.RateLimits
		}
		if cfg.Captcha == nil {
			cfg.Captcha = defaults.Captcha
		}
	}
	return nil
}

// App returns the application graph, the bridge the HTTP surface constructors
// (and embed.Runtime) build from.
func (e *Embedded) App() *app.App {
	if e == nil {
		return nil
	}
	return e.app
}

// ProviderRoutes selects provider-specific public routes for an embedded mount.
// Leave nil to derive from configured provider accounts when possible.
type ProviderRoutes struct {
	StripePortal bool
	Solana       bool
	Webhooks     bool
}

func (p ProviderRoutes) internal() routesurface.ProviderRoutes {
	// An explicit host selection is authoritative — enabling Solana enables its
	// signing routes, and the write surface stays on (no capability second-guess).
	return routesurface.ProviderRoutes{
		StripePortal:  p.StripePortal,
		Solana:        p.Solana,
		SolanaSigning: p.Solana,
		Webhooks:      p.Webhooks,
		SecretWrite:   true,
	}
}

// ActiveRouteSets returns the route groups of the most recently mounted HTTP
// surface (MountHandler), recorded at mount time; nil before any mount.
// Returns a copy. The typical host mounts once, so this reflects its live
// surface.
func (e *Embedded) ActiveRouteSets() []RouteSet {
	if e == nil {
		return nil
	}
	return append([]RouteSet(nil), e.activeRouteSets...)
}

// MountRouteSets resolves a selection (nil → EmbeddedDefaultRouteSets, empties
// dropped, deduped) and records it as the active HTTP surface for
// ActiveRouteSets() and capability discovery. The mount paths call it; hosts
// normally don't. Returns the resolved set.
func (e *Embedded) MountRouteSets(sets []RouteSet) []RouteSet {
	resolved := embedhttp.ResolveRouteSets(sets)
	if e != nil {
		e.activeRouteSets = resolved
	}
	return resolved
}

// Service returns the in-process billing API for embedded hosts.
func (e *Embedded) Service() (*service.Service, error) {
	if e == nil || e.app == nil {
		return nil, fmt.Errorf("embedded billing: app not initialized")
	}
	return service.New(e.app.Runtime)
}

// Control-plane bootstrap + accessor moved to the OPT-IN pkg/embedded/controlplane
// helper (#284): the embedded CORE no longer imports internal/controlplane (or,
// through it, AuthKit). Standalone/AuthKit hosts call
// controlplane.RunBootstrap(ctx, e.App(), controlplane.BootstrapOptions{...}) and
// controlplane.Get(e.App()) — BootstrapOptions/BootstrapResult are that
// package's own nameable, externally-constructible aliases (#747) for the
// internal/controlplane types of the same name, so an external host builds
// opts without reaching into (or being able to import) internal/controlplane.

func (e *Embedded) RunWorkers(ctx context.Context) error {
	if e == nil || e.app == nil || e.app.Runtime == nil {
		return fmt.Errorf("runtime is not initialized")
	}
	return e.app.Runtime.RunWorkers(ctx)
}

func (e *Embedded) Close(ctx context.Context) error {
	if e == nil || e.app == nil {
		return nil
	}
	return e.app.Close(ctx)
}

func (e *Embedded) Config() *config.Config {
	if e == nil || e.app == nil {
		return nil
	}
	return e.app.Config
}
