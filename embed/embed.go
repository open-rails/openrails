// Package embed runs the OpenRails engine in the host process and hands out
// the same *openrails.Client that openrails.NewRemote builds, wired to an
// in-process transport. Embedded versus standalone is a constructor choice;
// application code written against the Client does not change. The root
// openrails package stays engine-free; this package is the only public one
// that links the engine.
package embed

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/inprocess"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/pkg/cache"
)

// Options configures the embedded runtime.
type Options struct {
	// Config is built programmatically by the host; embedded construction never
	// runs config.Load, so Env and TestMode (sandbox or live) must be set
	// explicitly. Rate-limit and captcha defaults are seeded when left nil
	// unless Config.RateLimitsDisabled.
	Config *config.Config
	// PGXPool is the host-supplied database handle. Leave nil to open one from
	// Config.DB.
	PGXPool *pgxpool.Pool
	Redis   *redis.Client
	Cache   cache.Cache
	// River declares who owns the River job fleet. Required: use
	// RiverFromHost(bind) when the host owns River, RiverManagedByOpenRails()
	// to let OpenRails run its own.
	River RiverOwnership
	// RunWorkers starts the River workers on a goroutine owned by the Runtime
	// (stopped by Close), detached from the ctx passed to New. Leave false to
	// drive Runtime.RunWorkers yourself.
	RunWorkers bool
	// ConsoleAssets is the host-built admin console SPA rooted at index.html
	// (scripts/build-admin-console.sh); nil links no frontend bytes.
	ConsoleAssets fs.FS
	// StripeTransport is the test seam under the Stripe API choke point for
	// driving rail pushes against a fake Stripe. Refused with a live posture.
	// Process-wide: this does not independently route concurrent runtimes.
	StripeTransport http.RoundTripper
}

// Runtime is the in-process engine: Client() for the shared client, Handler()
// to mount the billing HTTP surface, RunWorkers/Close for lifecycle.
type Runtime struct {
	app *app.App
	svc *service.Service

	activeRouteSets        []RouteSet
	releaseStripeTransport func()

	workersCancel context.CancelFunc
	workersDone   chan error

	// handlerOnce memoizes the in-process route mux: it depends only on the
	// app graph, never on per-Client() options.
	handlerOnce sync.Once
	handler     http.Handler
}

func init() {
	app.HostGraph = func(runtime any) *app.App {
		if r, ok := runtime.(*Runtime); ok && r != nil {
			return r.app
		}
		return nil
	}
}

// New builds the engine. ctx bounds the wait for the database; nothing else
// does.
func New(ctx context.Context, opts Options) (*Runtime, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Config == nil {
		return nil, fmt.Errorf("openrails embed: config is required")
	}
	if !opts.River.declared() {
		return nil, ErrRiverRequired
	}
	if err := applyEmbeddedDefaults(opts.Config); err != nil {
		return nil, err
	}
	if opts.StripeTransport != nil && opts.Config.TestMode == config.CredentialPostureLive {
		return nil, fmt.Errorf("openrails embed: Options.StripeTransport is a test seam and is refused with config.TestMode=live")
	}
	application, err := app.BootstrapWithOptions(ctx, opts.Config, &app.BootstrapOptions{
		PGXPool: opts.PGXPool,
		Redis:   opts.Redis,
		Cache:   opts.Cache,
	})
	if err != nil {
		return nil, fmt.Errorf("bootstrap application: %w", err)
	}
	// Ordinary Client calls need the same provider/secret graph as the
	// standalone server; worker startup or mounting cannot be prerequisites.
	if err := application.Runtime.EnsureMerchantsService(ctx); err != nil {
		_ = application.Close(ctx)
		return nil, fmt.Errorf("initialize merchant services: %w", err)
	}
	application.ConsoleAssets = opts.ConsoleAssets

	r := &Runtime{app: application}
	if opts.StripeTransport != nil {
		r.releaseStripeTransport = stripeapi.InstallBaseTransport(opts.StripeTransport)
	}
	if err := r.bindRiver(ctx, opts.River); err != nil {
		_ = r.Close(ctx)
		return nil, err
	}
	// The out-of-River progress detector starts at construction: a host that
	// never calls RunWorkers is exactly the case that must be detectable.
	application.Runtime.StartRiverProgressMonitor(ctx)
	svc, err := service.New(application.Runtime)
	if err != nil {
		_ = r.Close(ctx)
		return nil, err
	}
	r.svc = svc
	if opts.RunWorkers {
		wctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		r.workersCancel = cancel
		r.workersDone = make(chan error, 1)
		go func() { r.workersDone <- application.Runtime.RunWorkers(wctx) }()
	}
	return r, nil
}

// applyEmbeddedDefaults enforces the posture embedded construction must declare
// and seeds the protective defaults config.Load applies.
func applyEmbeddedDefaults(cfg *config.Config) error {
	if strings.TrimSpace(cfg.Env) == "" {
		return fmt.Errorf("openrails embed: config.Env is required; embedded construction never runs config.Load's dev-like empty-Env default")
	}
	switch cfg.TestMode {
	case config.CredentialPostureSandbox, config.CredentialPostureLive:
	default:
		return fmt.Errorf("openrails embed: config.TestMode is required; set config.CredentialPostureSandbox or config.CredentialPostureLive explicitly")
	}
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

// Client returns the same typed client as NewRemote over the in-process
// operation transport. It is bound to the runtime's configured merchant, or to
// WithMerchantID on a multi-merchant runtime; an unbound client is refused.
func (r *Runtime) Client(options ...openrails.ClientOption) (*openrails.Client, error) {
	rt := r.app.Runtime
	r.handlerOnce.Do(func() { r.handler = newServiceHandler(rt) })
	defaults := []openrails.ClientOption{
		openrails.WithHTTPClient(&http.Client{Transport: inprocess.NewTransport(r.handler, rt.ConfiguredMerchant)}),
		openrails.WithTokenProvider(func(context.Context) (string, error) { return "in-process-host", nil }),
	}
	if id := rt.ConfiguredMerchant(); !id.IsZero() {
		defaults = append(defaults, openrails.WithMerchantID(id))
	}
	client, err := openrails.NewRemote(inprocessBaseURL, append(defaults, options...)...)
	if err != nil {
		return nil, err
	}
	switch bound := rt.ConfiguredMerchant(); {
	case client.MerchantID().IsZero():
		return nil, fmt.Errorf("openrails embed: runtime serves several merchants; bind the client with openrails.WithMerchantID")
	case !bound.IsZero() && client.MerchantID() != bound:
		return nil, fmt.Errorf("openrails embed: %s", merchantMismatchMsg(bound, client.MerchantID()))
	}
	return client, nil
}

// ActiveRouteSets returns the route groups of the most recently mounted HTTP
// surface; nil before any mount. It is the in-process twin of
// GET /v1/capabilities.
func (r *Runtime) ActiveRouteSets() []RouteSet {
	if r == nil {
		return nil
	}
	return append([]RouteSet(nil), r.activeRouteSets...)
}

func (r *Runtime) mountRouteSets(sets []RouteSet) []RouteSet {
	resolved := embedhttp.ResolveRouteSets(sets)
	r.activeRouteSets = resolved
	return resolved
}

// RunWorkers runs the River workers, blocking until ctx is done.
func (r *Runtime) RunWorkers(ctx context.Context) error {
	if r == nil || r.app == nil || r.app.Runtime == nil {
		return fmt.Errorf("openrails embed: runtime is not initialized")
	}
	return r.app.Runtime.RunWorkers(ctx)
}

// Close stops Options.RunWorkers workers (waiting for them up to ctx) and
// closes the app graph.
func (r *Runtime) Close(ctx context.Context) error {
	if r == nil || r.app == nil {
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
	if r.releaseStripeTransport != nil {
		defer r.releaseStripeTransport()
	}
	return r.app.Close(ctx)
}
