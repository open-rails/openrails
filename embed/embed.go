// Package embed is the heavy half of the unified OpenRails SDK (#338/#685): it
// runs the engine IN-PROCESS (pgx, river, the full pkg/embedded app graph) and
// hands out the SAME client implementation openrails.NewRemote builds, wired to
// an in-process transport (no socket). Embedded vs standalone is a constructor
// choice — host code written against openrails.Client does not change when the
// deployment flips.
//
// Package layout keeps remote-only consumers light: the root openrails package
// is interface + remote impl only; this package is the only one that links the
// engine.
package embed

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"sync"

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

// Option adjusts Options before the runtime is built (New's variadic tail).
type Option func(*Options)

// WithAdminConsole supplies the HOST-BUILT admin console SPA (#754): an fs.FS
// rooted at index.html, typically a 3-line `//go:embed all:dist` package in the
// host repo over a gitignored dist produced by openrails'
// scripts/build-admin-console.sh. Not passing this links ZERO frontend bytes;
// enabling admin_console without it is a boot error on the standalone surface.
func WithAdminConsole(assets fs.FS) Option {
	return func(o *Options) { o.ConsoleAssets = assets }
}

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

// Runtime is the in-process OpenRails engine plus its SDK adapter. It is the
// ONE entry point an embedding host needs: Client() for the unified interface,
// Handler() to mount the embedded HTTP surface, RunWorkers/Close for lifecycle.
type Runtime struct {
	emb *embedded.Embedded
	svc *service.Service

	workersCancel context.CancelFunc
	workersDone   chan error

	// handlerOnce/handler memoize the in-process route mux (#767): it depends
	// only on r.emb's *app.Runtime, never on per-Client() options, so building
	// it once and reusing it across every Client() call avoids re-running
	// RegisterServiceRoutes/RegisterImportRoutes on every call.
	handlerOnce sync.Once
	handler     http.Handler
}

// New builds the embedded runtime: the gin-free app graph (pkg/embedded.New),
// then the service facade the Client adapts. The variadic tail applies
// functional options (e.g. WithAdminConsole) on top of opts.
func New(ctx context.Context, opts Options, options ...Option) (*Runtime, error) {
	for _, opt := range options {
		if opt != nil {
			opt(&opts)
		}
	}
	if opts.Config == nil {
		return nil, fmt.Errorf("openrails embed: config is required")
	}
	emb, err := embedded.New(ctx, opts.Options)
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

// Client returns the same typed client as NewRemote over the in-process
// operation transport. Options, validation and errors follow the same path.
func (r *Runtime) Client(options ...ClientOption) (*openrails.Client, error) {
	config := &clientOptions{}
	for _, option := range options {
		if option != nil {
			option(config)
		}
	}
	rt := r.emb.App().Runtime
	r.handlerOnce.Do(func() { r.handler = newServiceHandler(rt) })
	defaults := []openrails.RemoteOption{
		openrails.WithHTTPClient(&http.Client{Transport: &inprocessTransport{handler: r.handler, rt: rt}}),
		openrails.WithTokenProvider(func(context.Context) (string, error) { return "in-process-host", nil }),
		openrails.WithCurrency(config.currency),
	}
	if id := rt.ConfiguredMerchant(); !id.IsZero() {
		defaults = append(defaults, openrails.WithMerchantID(id))
	}
	return openrails.NewRemote(inprocessBaseURL, append(defaults, config.remoteOptions...)...)
}

// Service exposes the underlying pkg/service facade for host code that wants
// engine-native types (identity.CustomerID etc.) instead of wire types.
func (r *Runtime) Service() *service.Service { return r.svc }

// Embedded exposes the underlying pkg/embedded app for advanced wiring
// (control plane attach, river client injection, embedded.MountHandler).
func (r *Runtime) Embedded() *embedded.Embedded { return r.emb }

// ActiveRouteSets returns the route groups of the most recently mounted HTTP
// surface (embedded.MountHandler); nil before any mount. It is the in-process
// twin of GET /v1/capabilities — same source.
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
