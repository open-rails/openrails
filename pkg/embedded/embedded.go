package embedded

import (
	"context"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"

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
	// config.Load, so none of Load's defaulting applies. In particular set
	// Config.TestMode EXPLICITLY: the zero value means LIVE credential posture
	// (standalone dev boots default to sandbox; embedded ones do not). New()
	// warns when TestMode is left false in a dev-like Env (#711).
	Config *config.Config
	// PGXPool is the host-supplied database handle (pgx/v5). The bun-era
	// *sql.DB option was removed with the ORM (#334).
	PGXPool *pgxpool.Pool
	Redis   *redis.Client
	Cache   cache.Cache
	// PaymentProviders lets embedding hosts supply one or more payment-provider
	// credential sets programmatically — the BOOT-CONFIG plane. Local provider
	// names are optional selectors; durable provider-account identity is
	// resolved from the provider itself.
	//
	// This is NOT the only way to arm providers (#699): a host that seeds
	// provider accounts + secrets through the merchant manifest (the
	// per-merchant secrets store) gets checkout, webhooks AND provider pulls
	// (refresh/probes/unknown-resolution) armed per merchant from the store,
	// with this boot plane as fallback. Store credentials win on conflict.
	PaymentProviders []PaymentProvider
}

type Embedded struct {
	app *app.App
	// activeRouteSets records the resolved route groups of the most recently
	// mounted HTTP surface (#623), for ActiveRouteSets() and capability
	// discovery. Written at mount time (boot), read while serving.
	activeRouteSets []RouteSet
}

func New(opts Options) (*Embedded, error) {
	if opts.Config == nil {
		return nil, fmt.Errorf("config is required")
	}
	rails, err := ApplyPaymentProviders(opts.PaymentProviders)
	if err != nil {
		return nil, err
	}

	// #711: warn on the embedded test_mode zero-value fail-open. Standalone
	// Load() defaults a dev-like boot to sandbox; embedded hosts build Config
	// programmatically, so a forgotten TestMode silently means LIVE credentials.
	// Warn-only (an explicit TestMode=false is indistinguishable from omission).
	if opts.Config.IsDev() && !opts.Config.TestMode {
		log.Warn("openrails embedded: Config.TestMode is false in a dev-like env — LIVE credential posture. " +
			"Standalone defaults dev boots to sandbox; set TestMode explicitly (true for sandbox, false if live is intended).")
	}

	// Build the application graph only; HTTP surfaces (StandaloneHandler /
	// NewHTTPHandler / MountHandler) are constructed from this App on demand.
	// (#711: the bootstrap.Options relay layer is gone — this calls the app
	// composition root directly.)
	application, err := app.BootstrapWithOptions(opts.Config, &app.BootstrapOptions{
		PGXPool: opts.PGXPool,
		Redis:   opts.Redis,
		Cache:   opts.Cache,
		Rails:   rails,
	})
	if err != nil {
		return nil, fmt.Errorf("bootstrap application: %w", err)
	}

	return &Embedded{app: application}, nil
}

// App returns the application graph, the bridge the HTTP surface constructors
// (and embed.Runtime) build from.
func (e *Embedded) App() *app.App {
	if e == nil {
		return nil
	}
	return e.app
}

// HTTPHandlerOptions controls which billing HTTP route groups are included in the returned handler.
//
// A zero RouteSets slice uses EmbeddedDefaultRouteSets.
//
// Note: billing health endpoints are not exposed in embedded mode.
// If a host wants billing readiness, call IsBillingReady and include it in the host's /readyz.
type HTTPHandlerOptions struct {
	RouteSets      []RouteSet
	ProviderRoutes *ProviderRoutes
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

// NewHTTPHandler returns a single mountable `http.Handler` for the selected route groups.
//
// Embedded routes live under `/billing/v1/*`.
func (e *Embedded) NewHTTPHandler(opts HTTPHandlerOptions) http.Handler {
	if e == nil || e.app == nil {
		return nil
	}
	active := e.MountRouteSets(opts.RouteSets)
	var providerRoutes *routesurface.ProviderRoutes
	if opts.ProviderRoutes != nil {
		v := opts.ProviderRoutes.internal()
		providerRoutes = &v
	}
	return embedhttp.FromApp(e.app).NewHTTPHandler(embedhttp.Options{
		RouteSets:          active,
		AdvertiseRouteSets: active,
		ProviderRoutes:     providerRoutes,
	})
}

// ActiveRouteSets returns the route groups of the most recently mounted HTTP
// surface (NewHTTPHandler / MountHandler), recorded at mount time; nil before
// any mount. Returns a copy. The typical host mounts once, so this reflects its
// live surface.
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
// controlplane.RunBootstrap(ctx, e.App(), opts) and controlplane.Get(e.App()).

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
