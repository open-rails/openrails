// Package embed is the heavy half of the unified OpenRails SDK (#338): it runs
// the engine IN-PROCESS (pgx, river, the full pkg/embedded app graph) and
// exposes the same openrails.Client interface that openrails.NewRemote serves
// over HTTP. Embedded vs standalone is a constructor choice — host code written
// against openrails.Client does not change when the deployment flips.
//
// Package layout keeps remote-only consumers light: the root openrails package
// is interface + remote impl only; this package is the only one that links the
// engine.
package embed

import (
	"context"
	"fmt"
	"net/http"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/pkg/embedded"
	"github.com/open-rails/openrails/pkg/service"
)

// Options configures the embedded runtime. It wraps pkg/embedded.Options
// (Config, PGXPool, Redis, Cache) and adds lifecycle switches.
type Options struct {
	embedded.Options

	// RunWorkers starts the River background workers on a goroutine owned by
	// the Runtime (stopped by Close). The worker context is detached from the
	// ctx passed to New (context.WithoutCancel) so a short-lived startup
	// context does not kill long-running workers; cancellation is Close's job.
	// Leave false to drive workers yourself via Runtime.RunWorkers.
	RunWorkers bool
}

// HandlerOptions selects the HTTP route groups for Runtime.Handler. It is
// pkg/embedded.HTTPHandlerOptions; zero value uses EmbeddedDefaultRouteSets.
type HandlerOptions = embedded.HTTPHandlerOptions

// ProviderRoutes selects provider-specific public routes for HandlerOptions.
type ProviderRoutes = embedded.ProviderRoutes

// RouteSet names a mountable billing HTTP route group.
type RouteSet = embedded.RouteSet

const (
	// RouteSetCheckout mounts buyer-facing products, prices, config, and checkout routes.
	RouteSetCheckout = embedded.RouteSetCheckout
	// RouteSetCustomer mounts customer-facing billing routes (/v1/me/*, /v1/customers/*).
	RouteSetCustomer = embedded.RouteSetCustomer
	// RouteSetMerchantAdmin mounts human merchant-admin customer/support routes.
	RouteSetMerchantAdmin = embedded.RouteSetMerchantAdmin
	// RouteSetCatalog mounts merchant catalog routes.
	RouteSetCatalog = embedded.RouteSetCatalog
	// RouteSetPaymentProviders mounts provider config and secret routes.
	RouteSetPaymentProviders = embedded.RouteSetPaymentProviders
	// RouteSetMerchantAPI mounts the host-internal service/API-key surface
	// (/billing/v1/merchant/*). Opt in for embedded hosts that want the same
	// service-credential surface as standalone; most embedded hosts use Client() instead.
	RouteSetMerchantAPI = embedded.RouteSetMerchantAPI
	// RouteSetWebhooks mounts merchant-scoped inbound webhook routes.
	RouteSetWebhooks = embedded.RouteSetWebhooks
)

var (
	// EmbeddedDefaultRouteSets is the default embedded HTTP surface: checkout,
	// customer, merchant_admin, catalog, and webhooks. It excludes
	// RouteSetPaymentProviders and RouteSetMerchantAPI (both opt-in for embedded hosts).
	EmbeddedDefaultRouteSets = append([]RouteSet(nil), embedded.EmbeddedDefaultRouteSets...)
	// StandaloneDefaultRouteSets is the full standalone HTTP surface, including
	// payment_providers and merchant_api in addition to EmbeddedDefaultRouteSets.
	StandaloneDefaultRouteSets = append([]RouteSet(nil), embedded.StandaloneDefaultRouteSets...)
)

// PaymentProvider is one embedded payment-provider credential set. It aliases
// pkg/embedded.PaymentProvider so hosts using embed.New can configure multiple
// provider accounts without importing the lower-level package.
type PaymentProvider = embedded.PaymentProvider

// Runtime is the in-process OpenRails engine plus its SDK adapter. It is the
// ONE entry point an embedding host needs: Client() for the unified interface,
// Handler() to mount the embedded HTTP surface, RunWorkers/Close for lifecycle.
type Runtime struct {
	emb *embedded.Embedded
	svc *service.Service

	workersCancel context.CancelFunc
	workersDone   chan error
}

// New builds the embedded runtime: the gin-free app graph (pkg/embedded.New),
// then the service facade the Client adapts.
func New(ctx context.Context, opts Options) (*Runtime, error) {
	if opts.Config == nil {
		return nil, fmt.Errorf("openrails embed: config is required")
	}
	emb, err := embedded.New(opts.Options)
	if err != nil {
		return nil, err
	}
	svc, err := emb.Service()
	if err != nil {
		_ = emb.Close(ctx)
		return nil, err
	}

	r := &Runtime{emb: emb, svc: svc}
	if opts.RunWorkers {
		wctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		r.workersCancel = cancel
		r.workersDone = make(chan error, 1)
		go func() { r.workersDone <- emb.RunWorkers(wctx) }()
	}
	return r, nil
}

// Client returns the openrails.Client adapter over the in-process engine. Each
// method transcribes the wire→service mapping of the corresponding standalone
// handler (internal/http/handlers/service_*.go), including the error→status
// mapping, so it is observably identical to NewRemote against the same engine
// (enforced by conformance_integration_test.go).
func (r *Runtime) Client(opts ...ClientOption) openrails.Client {
	c := &localClient{svc: r.svc}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c
}

// Service exposes the underlying pkg/service facade for host code that wants
// engine-native types (identity.CustomerID etc.) instead of wire types.
func (r *Runtime) Service() *service.Service { return r.svc }

// Embedded exposes the underlying pkg/embedded app for advanced wiring
// (control plane attach, river client injection, gin route registration via
// pkg/embedded/gin).
func (r *Runtime) Embedded() *embedded.Embedded { return r.emb }

// Handler returns the mountable embedded HTTP surface (/billing/v1/*) — a thin
// passthrough to pkg/embedded.NewHTTPHandler (which records the active route sets
// for ActiveRouteSets / capability discovery). The service-credential-authenticated
// /billing/v1/merchant/* surface is opt-in via embedded.RouteSetMerchantAPI; an
// embedded host normally uses Client() instead.
func (r *Runtime) Handler(opts HandlerOptions) http.Handler {
	return r.emb.NewHTTPHandler(opts)
}

// ActiveRouteSets returns the route groups of the most recently mounted HTTP
// surface (Handler, or pkg/embedded/gin MountHandler / RegisterAPI); nil before
// any mount. It is the in-process twin of GET /v1/capabilities — same source.
func (r *Runtime) ActiveRouteSets() []RouteSet {
	if r == nil {
		return nil
	}
	return r.emb.ActiveRouteSets()
}

// RunWorkers runs the River workers, blocking until ctx is done — a thin
// passthrough for hosts that did not set Options.RunWorkers.
func (r *Runtime) RunWorkers(ctx context.Context) error {
	return r.emb.RunWorkers(ctx)
}

// Close stops Options.RunWorkers workers (waiting for them up to ctx) and
// closes the app graph.
func (r *Runtime) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if r.workersCancel != nil {
		r.workersCancel()
		select {
		case <-r.workersDone:
		case <-ctx.Done():
		}
		r.workersCancel = nil
	}
	return r.emb.Close(ctx)
}
