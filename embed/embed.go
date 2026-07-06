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

// PaymentProvider is one embedded payment-provider credential set (the
// boot-config plane). It aliases pkg/embedded.PaymentProvider so hosts using
// embed.New can configure multiple provider accounts without importing the
// lower-level package. Hosts that seed provider accounts + secrets via the
// merchant manifest (UpsertMerchantConfig) don't need it: checkout, webhooks
// and provider pulls all arm per merchant from the secrets store (#699).
type PaymentProvider = embedded.PaymentProvider

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

// Client returns the unified openrails.Client over the in-process engine
// (#685): the SAME client implementation NewRemote builds, wired to an
// in-process transport that dispatches into the real neutral /v1/merchant
// handler — real auth gate (context-attached host principal), real merchant
// pinning, real RLS DB-conn middleware. No socket; one JSON round-trip per
// call. Parity with a standalone deployment is structural (one implementation),
// enforced by conformance_integration_test.go.
//
// One interface method (SetCustomerSpendDelegations) is PERMANENTLY served by
// the transcribed localClient — see unifiedClient and the localClient doc for
// the authz reasoning. The returned Client also always implements
// SingleAdmitter (embedded-only single Admit, no wire counterpart) — reach it
// via a type assertion.
//
// Built-in RemoteOptions (transport, token provider, currency, timeout) are
// applied first; any host-supplied opts carrying WithRemoteOptions are applied
// LAST, so a host can override any one of them (e.g. opt back into a deadline)
// without embed re-exposing each knob individually.
func (r *Runtime) Client(opts ...ClientOption) openrails.Client {
	c := &localClient{svc: r.svc, rt: r.emb.App().Runtime}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	rt := r.emb.App().Runtime
	r.handlerOnce.Do(func() { r.handler = newServiceHandler(rt) })
	transport := &inprocessTransport{handler: r.handler, rt: rt}
	builtins := []openrails.RemoteOption{
		openrails.WithHTTPClient(&http.Client{Transport: transport}),
		// The credential is the context-attached host principal; the bearer is a
		// placeholder the in-process gate never consults.
		openrails.WithTokenProvider(func(context.Context) (string, error) { return "in-process-host", nil }),
		openrails.WithCurrency(c.currency),
		// In-process calls have no network hop to bound: disable the remote's
		// default 2s per-call context deadline so calls are bounded ONLY by the
		// caller's own ctx (#767 — the deadline previously fired regardless of
		// the injected http.Client, silently truncating every embedded call).
		openrails.WithTimeout(0),
	}
	remote := openrails.NewRemote(inprocessBaseURL, append(builtins, c.remoteOpts...)...)
	return &unifiedClient{Client: remote, localClient: c}
}

// unifiedClient is the embedded SDK client (#685): the embedded
// openrails.Client (the remote implementation over the in-process transport)
// serves 20/21 interface methods; *localClient keeps the PERMANENT
// transcription (SetCustomerSpendDelegations — customer-treasury auth surface,
// see the localClient doc) plus the embedded-only single-Admit extra, which has
// no /v1/merchant wire counterpart.
type unifiedClient struct {
	openrails.Client
	*localClient
}

// SetCustomerSpendDelegations stays transcribed PERMANENTLY: its HTTP surface
// is the delegated customer-treasury family (/v1/customers/*), whose gate
// requires a credential that IS the customer — a mapping the merchant host
// principal cannot honestly satisfy (see the localClient doc). Explicit
// forward resolves the embedding conflict.
func (c *unifiedClient) SetCustomerSpendDelegations(ctx context.Context, customerID string, delegations []openrails.SpendDelegationInput) error {
	return c.localClient.SetCustomerSpendDelegations(ctx, customerID, delegations)
}

// Verify is the authenticated readiness probe (see openrails.Verify), running
// through the in-process transport + real auth gate.
func (c *unifiedClient) Verify(ctx context.Context) error {
	return openrails.Verify(ctx, c.Client)
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
