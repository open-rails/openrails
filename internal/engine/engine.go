// Package engine is the in-process OpenRails engine behind openrails.New. It
// builds the application graph from Config and Deps and owns its lifecycle,
// HTTP routes, River composition and the opt-in control plane.
//
// River is required (#895): renewals, credit expiry, invoices, webhook
// reconciliation and provider intents all run as River jobs. With River
// absent every read keeps answering and the money silently stops. The fleet is
// either OpenRails-managed (Start runs it) or host-owned (the host composes
// RiverJobs into its one fleet). OpenRails also watches the fleet's progress
// from outside River and reports a stall through Probes.
package engine

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/routebundle"
	"github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/pkg/merchant"
	admin "github.com/open-rails/openrails/web/admin"
)

// Engine is one embedded engine.
type Engine struct {
	App *app.App
	svc *service.Service

	merchant config.MerchantDeclaration
	http     *config.HTTPConfig

	mu          sync.Mutex
	closed      bool
	stopWorkers func()
	routes      []routebundle.Route

	handlerOnce sync.Once
	handler     http.Handler

	closeOnce sync.Once
	closeErr  error
}

// Of returns the engine behind an openrails.Client, nil for a remote one. The
// root package sets it; operator tooling inside the module uses it.
var Of func(client any) *Engine

// Graph returns the application graph behind an embedded openrails.Client.
func Graph(client any) *app.App {
	if Of == nil {
		return nil
	}
	if e := Of(client); e != nil {
		return e.App
	}
	return nil
}

// New builds the engine. ctx bounds the wait for the database; nothing else
// does. Only Postgres fails construction: Vault login, PSP posture and Redis
// recover in the background and fail only the features that need them.
func New(ctx context.Context, cfg config.Config, deps config.Deps) (*Engine, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validate(&cfg, deps); err != nil {
		return nil, err
	}
	auth := integration(deps)
	httpCfg, err := httpConfig(cfg, auth)
	if err != nil {
		return nil, err
	}
	if deps.Postgres != nil && (cfg.DB == nil || cfg.DB.GetConnectionString() == "") {
		url := deps.Postgres.Config().ConnString()
		if url == "" {
			return nil, fmt.Errorf("openrails: Config.DB is required when Deps.Postgres was not built from a connection string")
		}
		db := config.DBConfig{}
		if cfg.DB != nil {
			db = *cfg.DB
		}
		db.URL = url
		cfg.DB = &db
	}
	bootstrap := &app.BootstrapOptions{
		HostRiver:        cfg.River.HostOwned(),
		RiverSchema:      cfg.RiverSchema,
		PGXPool:          deps.Postgres,
		Redis:            deps.Redis,
		Cache:            deps.Cache,
		UserDirectory:    userDirectory(deps),
		UsernameResolver: usernameResolver(deps),
		StripeTransport:  deps.StripeTransport,
		NMITransport:     deps.NMITransport,
		DNSResolver:      deps.DNSResolver,
		Clock:            deps.Clock,
	}
	if !cfg.River.HostOwned() && strings.TrimSpace(bootstrap.RiverSchema) == "" {
		bootstrap.RiverSchema = config.DefaultRiverSchema
	}
	application, err := app.BootstrapWithOptions(ctx, &cfg, bootstrap)
	if err != nil {
		return nil, fmt.Errorf("bootstrap application: %w", err)
	}
	e := &Engine{App: application, merchant: cfg.Merchant, http: httpCfg}
	fail := func(err error) (*Engine, error) {
		_ = e.Close(ctx)
		return nil, err
	}
	rt := application.Runtime
	rt.VaultClient = deps.Vault
	if err := loadProviderCredentialSnapshot(ctx, rt, deps.ProviderCredentials); err != nil {
		return fail(err)
	}
	// Client operations need the same provider and secret graph as the
	// standalone server; neither workers nor routes are prerequisites.
	if err := rt.EnsureMerchantsService(ctx); err != nil {
		return fail(fmt.Errorf("initialize merchant services: %w", err))
	}
	application.ConsoleAssets = admin.FS()
	rt.Auth = auth
	signerPending, err := configureMerchant(ctx, application, e.merchant)
	if err != nil {
		return fail(err)
	}
	var declared []merchant.ID
	if e.merchant.Slug != "" {
		if m, err := rt.Merchants.GetBySlug(ctx, e.merchant.Slug); err == nil {
			declared = append(declared, m.ID)
		}
	}
	if cfg.ControlPlane != nil {
		if err := attachControlPlane(ctx, application, *cfg.ControlPlane, deps); err != nil {
			return fail(err)
		}
	}
	if !cfg.River.HostOwned() {
		if _, err := rt.GetBillingPeriodicJobs(ctx); err != nil {
			return fail(fmt.Errorf("build billing periodic jobs: %w", err))
		}
		rt.StartRiverProgressMonitor(ctx)
	}
	if e.svc, err = service.New(rt); err != nil {
		return fail(err)
	}
	declaration := e.merchant
	rt.ApproveSolanaSigner = func(ctx context.Context, mid merchant.ID, key string) error {
		return approveSolanaSigner(ctx, application, declaration, mid, key)
	}
	if signerPending {
		confirmSigner(application, declaration)
	}
	rt.StartProviderPosture(declared...)
	return e, nil
}

// validate enforces what embedded construction must declare and seeds the
// protective defaults the standalone loader applies. Every refusal happens
// before any resource opens.
func validate(cfg *config.Config, deps config.Deps) error {
	switch cfg.TestMode {
	case config.CredentialPostureSandbox, config.CredentialPostureLive:
	default:
		return fmt.Errorf("openrails: Config.TestMode is required; set Sandbox or Live explicitly")
	}
	// Unset would run fail-closed readonly: renewals, retries and refunds
	// would silently never reach a provider. The host states it.
	switch mode := strings.ToLower(strings.TrimSpace(cfg.ProviderWriteMode)); mode {
	case config.ProviderWriteModeFull, config.ProviderWriteModeLimited, config.ProviderWriteModeReadOnly:
	case "":
		return fmt.Errorf("openrails: Config.ProviderWriteMode is required; set full, limited or readonly explicitly (readonly never charges: renewals, retries and refunds wait)")
	default:
		return fmt.Errorf("openrails: Config.ProviderWriteMode %q is invalid; use full, limited or readonly", cfg.ProviderWriteMode)
	}
	if !reflect.ValueOf(cfg.Merchant).IsZero() && strings.TrimSpace(cfg.Merchant.Slug) == "" {
		return fmt.Errorf("openrails: Config.Merchant.Slug is required")
	}
	cfg.RiverSchema = strings.ToLower(strings.TrimSpace(cfg.RiverSchema))
	switch cfg.River {
	case "", config.RiverManaged:
		if cfg.RiverSchema != "" && !validIdentifier(cfg.RiverSchema) {
			return fmt.Errorf("openrails: Config.RiverSchema %q is not a valid schema name", cfg.RiverSchema)
		}
	case config.RiverHostOwned:
		if cfg.RiverSchema != "" {
			return fmt.Errorf("openrails: Config.RiverSchema applies to managed River; a host-owned fleet keeps River's tables in its client's schema")
		}
	default:
		return fmt.Errorf("openrails: Config.River %q is invalid; use RiverManaged or RiverHostOwned", cfg.River)
	}
	if cfg.TestMode == config.CredentialPostureLive {
		for name, set := range map[string]bool{
			"StripeTransport": deps.StripeTransport != nil, "NMITransport": deps.NMITransport != nil,
			"DNSResolver": deps.DNSResolver != nil, "Clock": deps.Clock != nil,
		} {
			if set {
				return fmt.Errorf("openrails: Deps.%s is a test seam and is refused with TestMode live", name)
			}
		}
	}
	if (deps.UserExists == nil) != (deps.UserEmail == nil) {
		return fmt.Errorf("openrails: set Deps.UserExists and Deps.UserEmail together")
	}
	if deps.Authenticate == nil && (deps.Authorize != nil || deps.RecentSignIn != nil) {
		return fmt.Errorf("openrails: Deps.Authorize and Deps.RecentSignIn require Deps.Authenticate")
	}
	if len(deps.ProviderCredentials) > 0 {
		for _, rails := range cfg.Merchant.PSPs {
			for _, account := range rails {
				if len(account.Secrets) > 0 {
					return fmt.Errorf("openrails: supply snapshot credentials through either Deps.ProviderCredentials or Config.Merchant, not both")
				}
			}
		}
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

func validIdentifier(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i, c := range s {
		if !(c == '_' || c >= 'a' && c <= 'z' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// httpConfig copies Config.HTTP, defaulting native customer routes to the
// declared merchant, and validates it against the host's authentication.
func httpConfig(cfg config.Config, auth *billingauth.Integration) (*config.HTTPConfig, error) {
	if cfg.HTTP == nil {
		return nil, nil
	}
	out := *cfg.HTTP
	out.CustomerRoutes = append([]config.CustomerRoutesConfig(nil), cfg.HTTP.CustomerRoutes...)
	for i := range out.CustomerRoutes {
		if out.CustomerRoutes[i].Authenticate == nil && strings.TrimSpace(out.CustomerRoutes[i].Merchant) == "" {
			out.CustomerRoutes[i].Merchant = cfg.Merchant.Slug
		}
	}
	if out.CookieOrigin != "" {
		if _, err := billingauth.CookieAuthentication(out.CookieOrigin); err != nil {
			return nil, fmt.Errorf("openrails: Config.HTTP.CookieOrigin: %w", err)
		}
	}
	if cfg.ControlPlane != nil {
		if out.Checkout || out.MerchantAdmin || out.Catalog || out.MerchantConfig || out.MerchantAPI {
			return nil, fmt.Errorf("openrails: with Config.ControlPlane, Routes serves the standalone surface; Config.HTTP may only add CustomerRoutes")
		}
		for _, routes := range out.CustomerRoutes {
			if routes.Authenticate == nil {
				return nil, fmt.Errorf("openrails: with Config.ControlPlane, CustomerRoutes need their own Authenticate")
			}
		}
		return &out, nil
	}
	if err := embedhttp.ValidateHTTPConfig(&out, auth); err != nil {
		return nil, err
	}
	return &out, nil
}

// ConfiguredMerchant is the declared merchant's ID, zero when unbound.
func (e *Engine) ConfiguredMerchant() merchant.ID { return e.App.Runtime.ConfiguredMerchant() }

// Start starts OpenRails' workers on goroutines Close stops: the managed River
// fleet and the loops that run outside River. With a host-owned fleet, compose
// RiverJobs into it first; the host starts it.
func (e *Engine) Start(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return fmt.Errorf("openrails: client is closed")
	}
	if e.stopWorkers != nil {
		return fmt.Errorf("openrails: already started")
	}
	wctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop, err := e.App.Runtime.StartWorkers(wctx)
	if err != nil {
		cancel()
		return err
	}
	e.stopWorkers = func() { cancel(); stop() }
	return nil
}

// Close stops the workers Start started, then closes the engine. The host's
// pool, Redis client and Vault client stay open.
func (e *Engine) Close(ctx context.Context) error {
	e.closeOnce.Do(func() {
		e.mu.Lock()
		e.closed = true
		stop := e.stopWorkers
		e.stopWorkers = nil
		e.mu.Unlock()
		if stop != nil {
			stop()
		}
		e.closeErr = e.App.Close(ctx)
	})
	return e.closeErr
}
