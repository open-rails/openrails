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
	"github.com/open-rails/openrails/internal/catalogscope"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/inprocess"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/cache"
)

// Options configures the embedded runtime.
type Options struct {
	// HTTP configures the externally mounted surface once. Nil disables HTTP.
	HTTP *HTTPConfig

	// DelegatedAuthenticator verifies explicit customer credentials for Client
	// self-service calls and is the default verifier for customer HTTP mounts.
	// Use the existing embed/authkit bridge; ambient host sessions confer no authority.
	DelegatedAuthenticator billingauth.DelegatedAuthenticator
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
	// River declares who owns the job fleet. The zero value uses an
	// OpenRails-managed client in public. RiverFromHost transfers ownership
	// to the host; RiverManagedByOpenRails optionally selects another schema.
	River RiverOwnership
	// RunWorkers is managed-only. It starts workers on a goroutine owned by the Runtime
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
	// UserDirectory and UsernameResolver are optional host identity adapters.
	// OpenRails does not assume ownership of AuthKit's profiles schema; hosts
	// opt in explicitly when they need notification email or CCBill username
	// resolution.
	UserDirectory    openrails.UserDirectory
	UsernameResolver openrails.UsernameResolver
}

// Runtime is the in-process engine: Client() for the shared client, Handler()
// to mount the billing HTTP surface, RunWorkers/Close for lifecycle.
type Runtime struct {
	httpConfig *HTTPConfig

	delegatedAuthenticator billingauth.DelegatedAuthenticator
	app                    *app.App
	svc                    *service.Service

	activeRouteSets        []RouteSet
	releaseStripeTransport func()

	closeOnce sync.Once
	closeErr  error

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
	if err := validateHTTPConfig(opts.HTTP, opts.DelegatedAuthenticator); err != nil {
		return nil, err
	}
	if opts.River.host && opts.RunWorkers {
		return nil, fmt.Errorf("openrails embed: host-owned River must compose RiverJobs with riverkit.New before host startup; Options.RunWorkers is managed-only")
	}
	riverSchema, err := opts.River.managedSchema(opts.Config.DB.SchemaName())
	if err != nil {
		return nil, err
	}
	if err := applyEmbeddedDefaults(opts.Config); err != nil {
		return nil, err
	}
	if opts.StripeTransport != nil && opts.Config.TestMode == config.CredentialPostureLive {
		return nil, fmt.Errorf("openrails embed: Options.StripeTransport is a test seam and is refused with config.TestMode=live")
	}
	application, err := app.BootstrapWithOptions(ctx, opts.Config, &app.BootstrapOptions{
		HostRiver:        opts.River.host,
		PGXPool:          opts.PGXPool,
		RiverSchema:      riverSchema,
		Redis:            opts.Redis,
		Cache:            opts.Cache,
		UserDirectory:    opts.UserDirectory,
		UsernameResolver: opts.UsernameResolver,
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

	r := &Runtime{app: application, delegatedAuthenticator: opts.DelegatedAuthenticator}
	if opts.HTTP != nil {
		cfg := *opts.HTTP
		r.httpConfig = &cfg
	}
	if opts.StripeTransport != nil {
		r.releaseStripeTransport = stripeapi.InstallBaseTransport(opts.StripeTransport)
	}
	if !opts.River.host {
		if _, err := application.Runtime.GetBillingPeriodicJobs(ctx); err != nil {
			_ = r.Close(ctx)
			return nil, fmt.Errorf("build billing periodic jobs: %w", err)
		}
		application.Runtime.StartRiverProgressMonitor(ctx)
	}
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
	return r.newClient("", options...)
}

// CatalogClient creates a restricted creator client for a host-authenticated
// subject. The subject is opaque; never take it from an untrusted request body
// or impersonate the owner read from a product row. Administrators use Client.
// Options may select merchant, currency and timeout; this constructor always
// retains its restricted in-process transport and credential.
func (r *Runtime) CatalogClient(subject string, options ...openrails.ClientOption) (*openrails.Client, error) {
	if err := catalogscope.ValidateSubject(subject); err != nil {
		return nil, err
	}
	return r.newClient(subject, append(options, openrails.WithOwnCatalog())...)
}

func (r *Runtime) newClient(subject string, options ...openrails.ClientOption) (*openrails.Client, error) {
	rt := r.app.Runtime
	r.handlerOnce.Do(func() { r.handler = newServiceHandler(rt, r.delegatedAuthenticator) })
	var transport http.RoundTripper
	var hostCapability string
	if subject == "" {
		transport, hostCapability = inprocess.NewTransport(r.handler, rt.ConfiguredMerchant)
	} else {
		var err error
		transport, hostCapability, err = inprocess.NewCatalogTransport(r.handler, rt.ConfiguredMerchant, subject)
		if err != nil {
			return nil, err
		}
	}
	defaults := []openrails.ClientOption{
		openrails.WithHTTPClient(&http.Client{Transport: transport}),
		openrails.WithTokenProvider(func(context.Context) (string, error) { return hostCapability, nil }),
	}
	if id := rt.ConfiguredMerchant(); !id.IsZero() {
		defaults = append(defaults, openrails.WithMerchantID(id))
	}
	clientOptions := append(defaults, options...)
	if subject != "" {
		// Options may select a merchant/currency/timeout, but cannot replace the
		// restricted host transport or inherit an administrator's credential.
		clientOptions = append(clientOptions,
			openrails.WithHTTPClient(&http.Client{Transport: transport}),
			openrails.WithTokenProvider(func(context.Context) (string, error) { return hostCapability, nil }),
			openrails.WithOwnCatalog(),
		)
	}
	client, err := openrails.NewRemote(inprocessBaseURL, clientOptions...)
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
	r.closeOnce.Do(func() {
		if r.workersCancel != nil {
			r.workersCancel()
			<-r.workersDone // join before closing resources even if shutdown ctx was canceled
			r.workersCancel = nil
		}
		if r.releaseStripeTransport != nil {
			defer r.releaseStripeTransport()
		}
		r.closeErr = r.app.Close(ctx)
	})
	return r.closeErr
}
