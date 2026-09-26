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

	vaultapi "github.com/hashicorp/vault/api"
	"github.com/jonboulle/clockwork"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/inprocess"
	"github.com/open-rails/openrails/internal/merchanttarget"
	"github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/cache"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Options configures the embedded runtime.
type Options struct {
	// VaultClient is an optional borrowed, authenticated client. The host owns
	// its renewal and lifetime; the Runtime does not revoke it on Close.
	VaultClient *vaultapi.Client
	// ProviderCredentials is an immutable credential dependency for existing
	// merchants, separate from metadata provisioning and HTTP exposure.
	ProviderCredentials []ProviderCredentialSnapshot
	// Auth supplies provider-neutral authentication and live authorization for
	// both private Client operations and any explicitly published HTTP routes.
	Auth *billingauth.Integration
	// Merchant declares this runtime's billing merchant and optional PSP identities.
	// Reconciliation finishes before HTTP configuration and worker startup.
	Merchant *MerchantDeclaration

	// HTTP configures the externally mounted surface once. Leave nil for a
	// headless runtime. Merchant slugs are resolved when routes are materialized.
	HTTP *HTTPConfig

	// DelegatedAuthenticator verifies explicit customer credentials for Client
	// self-service calls and is the default verifier for customer HTTP mounts.
	// Use billingauth.NewIntegration with the host verifier and explicit mappings.
	DelegatedAuthenticator billingauth.DelegatedAuthenticator
	// Config is built programmatically by the host; embedded construction never
	// runs config.Load, so TestMode (sandbox or live) and ProviderWriteMode
	// (full, limited or readonly) must be set explicitly. Rate-limit and captcha defaults are seeded when left nil
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
	// Scoped to this runtime; borrowed and never closed by OpenRails.
	StripeTransport http.RoundTripper
	// NMITransport is the test seam under the NMI client for driving the real
	// endpoints against a fake wire. It exempts nothing from sandbox posture.
	// Refused with a live posture.
	NMITransport http.RoundTripper
	// Clock is the test seam for engine time: renewal due dates, retry
	// schedules and entitlement windows read it. River scheduling and webhook
	// signature tolerance stay on wall time. Refused with a live posture.
	Clock clockwork.Clock
	// UserDirectory and UsernameResolver are optional host identity adapters.
	// OpenRails does not assume ownership of AuthKit's profiles schema; hosts
	// opt in explicitly when they need notification email or CCBill username
	// resolution.
	UserDirectory    openrails.UserDirectory
	UsernameResolver openrails.UsernameResolver
}

// Runtime is the in-process engine: Client() for the shared client, HTTPRoutes()
// to mount the billing HTTP surface, RunWorkers/Close for lifecycle.
type Runtime struct {
	httpMu     sync.Mutex
	httpConfig *HTTPConfig
	httpFrozen bool
	httpRoutes []HTTPRoute
	httpBuilt  bool
	closed     bool

	delegatedAuthenticator billingauth.DelegatedAuthenticator
	app                    *app.App
	svc                    *service.Service

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
// does. Only Postgres can fail construction: Vault login, PSP posture checks
// and Redis run in the background and fail only the features that need them
// (503) until they answer. Register Probes with the host's dependency
// supervisor to see them.
func New(ctx context.Context, opts Options) (*Runtime, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Config == nil {
		return nil, fmt.Errorf("openrails embed: config is required")
	}
	if err := validateMerchantDeclaration(opts.Merchant); err != nil {
		return nil, err
	}
	if err := embedhttp.ValidateHTTPConfig(opts.HTTP, opts.Auth); err != nil {
		return nil, err
	}
	if opts.River.host && opts.RunWorkers {
		return nil, fmt.Errorf("openrails embed: host-owned River must compose RiverJobs with riverhelpers.New before host startup; Options.RunWorkers is managed-only")
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
	if opts.NMITransport != nil && opts.Config.TestMode == config.CredentialPostureLive {
		return nil, fmt.Errorf("openrails embed: Options.NMITransport is a test seam and is refused with config.TestMode=live")
	}
	if opts.Clock != nil && opts.Config.TestMode == config.CredentialPostureLive {
		return nil, fmt.Errorf("openrails embed: Options.Clock is a test seam and is refused with config.TestMode=live")
	}
	application, err := app.BootstrapWithOptions(ctx, opts.Config, &app.BootstrapOptions{
		HostRiver:        opts.River.host,
		PGXPool:          opts.PGXPool,
		RiverSchema:      riverSchema,
		Redis:            opts.Redis,
		Cache:            opts.Cache,
		UserDirectory:    opts.UserDirectory,
		UsernameResolver: opts.UsernameResolver,
		StripeTransport:  opts.StripeTransport,
		NMITransport:     opts.NMITransport,
		Clock:            opts.Clock,
	})
	if err != nil {
		return nil, fmt.Errorf("bootstrap application: %w", err)
	}
	application.Runtime.VaultClient = opts.VaultClient
	if len(opts.ProviderCredentials) > 0 && opts.Merchant != nil {
		for _, rails := range opts.Merchant.Config.PSPs {
			for _, provider := range rails {
				if len(provider.Secrets) > 0 {
					_ = application.Close(ctx)
					return nil, fmt.Errorf("supply snapshot credentials through either ProviderCredentials or Merchant.Config, not both")
				}
			}
		}
	}
	if err := loadProviderCredentialSnapshot(ctx, application.Runtime, opts.ProviderCredentials); err != nil {
		_ = application.Close(ctx)
		return nil, err
	}
	// Ordinary Client calls need the same provider/secret graph as the
	// standalone server; worker startup or mounting cannot be prerequisites.
	if err := application.Runtime.EnsureMerchantsService(ctx); err != nil {
		_ = application.Close(ctx)
		return nil, fmt.Errorf("initialize merchant services: %w", err)
	}
	application.ConsoleAssets = opts.ConsoleAssets
	if opts.Auth != nil {
		copy := *opts.Auth
		application.Runtime.Auth = &copy
	}

	r := &Runtime{app: application, delegatedAuthenticator: opts.DelegatedAuthenticator}
	signerPending, err := configureMerchant(ctx, application, opts.Merchant)
	if err != nil {
		_ = r.Close(ctx)
		return nil, err
	}
	var declared []merchant.ID
	if opts.Merchant != nil {
		if m, err := application.Runtime.Merchants.GetBySlug(ctx, opts.Merchant.Slug); err == nil {
			declared = append(declared, m.ID)
		}
	}
	if opts.HTTP != nil {
		if err := r.configureHTTP(*opts.HTTP); err != nil {
			_ = r.Close(ctx)
			return nil, err
		}
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
	declaration := opts.Merchant
	application.Runtime.ApproveSolanaSigner = func(ctx context.Context, mid merchant.ID, key string) error {
		return approveSolanaSigner(ctx, application, declaration, mid, key)
	}
	if signerPending {
		confirmSigner(application, opts.Merchant)
	}
	application.Runtime.StartProviderPosture(declared...)
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
	switch cfg.TestMode {
	case config.CredentialPostureSandbox, config.CredentialPostureLive:
	default:
		return fmt.Errorf("openrails embed: config.TestMode is required; set config.CredentialPostureSandbox or config.CredentialPostureLive explicitly")
	}
	// Unset would run fail-closed readonly: renewals, retries and refunds
	// would silently never reach a provider. The host states it.
	switch mode := strings.ToLower(strings.TrimSpace(cfg.ProviderWriteMode)); mode {
	case config.ProviderWriteModeFull, config.ProviderWriteModeLimited, config.ProviderWriteModeReadOnly:
	case "":
		return fmt.Errorf("openrails embed: config.ProviderWriteMode is required; set full, limited or readonly explicitly (readonly never charges: renewals, retries and refunds wait)")
	default:
		return fmt.Errorf("openrails embed: config.ProviderWriteMode %q is invalid; use full, limited or readonly", cfg.ProviderWriteMode)
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
// operation transport. The configured merchant is an immutable default; an
// unrestricted runtime also supports explicit per-operation merchant selectors.
func (r *Runtime) Client(options ...openrails.ClientOption) (*openrails.Client, error) {
	rt := r.app.Runtime
	r.handlerOnce.Do(func() { r.handler = newServiceHandler(rt, r.delegatedAuthenticator) })
	transport, hostCapability := inprocess.NewTransportWithResolver(r.handler, rt.ConfiguredMerchant, func(ctx context.Context, request *http.Request) (billingauth.Target, error) {
		return merchanttarget.Resolve(ctx, request, rt.Merchants, rt.ConfiguredMerchant(), "")
	})
	defaults := []openrails.ClientOption{
		openrails.WithHTTPClient(&http.Client{Transport: transport}),
		openrails.WithTokenProvider(func(context.Context) (string, error) { return hostCapability, nil }),
	}
	if id := rt.ConfiguredMerchant(); !id.IsZero() {
		defaults = append(defaults, openrails.WithMerchantID(id))
	}
	return openrails.NewRemote(inprocessBaseURL, append(defaults, options...)...)
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
		r.httpMu.Lock()
		r.closed = true
		r.httpMu.Unlock()
		if r.workersCancel != nil {
			r.workersCancel()
			<-r.workersDone // join before closing resources even if shutdown ctx was canceled
			r.workersCancel = nil
		}
		r.closeErr = r.app.Close(ctx)
	})
	return r.closeErr
}
