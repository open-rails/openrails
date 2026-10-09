// Package engine is the in-process OpenRails engine behind openrails.New. It
// builds the application graph from Config and Deps and owns its lifecycle,
// HTTP routes and River composition.
//
// River is required (#895): renewals, credit expiry, invoices, webhook
// reconciliation and provider intents all run as River jobs. With River
// absent every read keeps answering and the money silently stops. Start runs
// OpenRails' own River client, or takes the host's fleet built with
// RiverJobs. OpenRails also watches the fleet's progress from outside River
// and reports a stall through Probes.
package engine

import (
	"context"

	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/routebundle"
	"github.com/open-rails/openrails/internal/service"
	admin "github.com/open-rails/openrails/web/admin"
)

// Engine is one embedded engine.
type Engine struct {
	App *app.App
	svc *service.Service

	merchant config.MerchantDeclaration

	mu          sync.Mutex
	closed      bool
	stopWorkers func()
	routes      map[string][]routebundle.Route

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

// New builds the engine. It first creates or upgrades OpenRails' tables, this
// month's partitions and River's tables (Migrate's work); the control plane's
// AuthKit migrates itself as it is built. ctx bounds the wait for the
// database; nothing else does. Only Postgres and a refused Config.Catalog fail
// construction: Vault login, PSP posture, Redis and a declared catalog's
// unconfirmed provider references recover in the background (see Ready and
// Probes).
func New(ctx context.Context, cfg config.Config, deps config.Deps) (*Engine, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validate(&cfg, deps); err != nil {
		return nil, err
	}
	catalogDoc, err := declaredCatalog(cfg)
	if err != nil {
		return nil, err
	}
	consoleAssets := deps.ConsoleAssets
	if consoleAssets == nil {
		consoleAssets = admin.FS()
	}
	if deps.Postgres != nil && (cfg.DB == nil || config.DBConnectionString(cfg.DB) == "") {
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
		PGXPool:         deps.Postgres,
		Redis:           deps.Redis,
		StripeTransport: deps.StripeTransport,
		NMITransport:    deps.NMITransport,
		DNSResolver:     deps.DNSResolver,
		Clock:           deps.Clock,
		EmailSender:     deps.Email,
		Contacts:        deps.Contacts,
	}
	application, err := app.BootstrapWithOptions(ctx, &cfg, bootstrap)
	if err != nil {
		return nil, fmt.Errorf("bootstrap application: %w", err)
	}
	e := &Engine{App: application, merchant: cfg.Merchant}
	fail := func(err error) (*Engine, error) {
		_ = e.Close(ctx)
		return nil, err
	}
	rt := application.Runtime
	rt.VaultClient = deps.Vault
	// Client operations need the same provider and secret graph as the
	// standalone server; neither workers nor routes are prerequisites.
	if err := rt.EnsureMerchantsService(ctx); err != nil {
		return fail(fmt.Errorf("initialize merchant services: %w", err))
	}
	application.ConsoleAssets = consoleAssets
	signerPending, err := configureMerchant(ctx, application, e.merchant)
	if err != nil {
		return fail(err)
	}
	var declared []billing.MerchantID
	if e.merchant.Slug != "" {
		if m, err := rt.Merchants.GetBySlug(ctx, e.merchant.Slug); err == nil {
			declared = append(declared, m.ID)
		}
	}
	if _, err := rt.GetBillingPeriodicJobs(ctx); err != nil {
		return fail(fmt.Errorf("build billing periodic jobs: %w", err))
	}
	if e.svc, err = service.New(rt); err != nil {
		return fail(err)
	}
	if catalogDoc != nil {
		if err := e.applyDeclaredCatalog(ctx, *catalogDoc); err != nil {
			return fail(err)
		}
	}
	declaration := e.merchant
	rt.ApproveSolanaSigner = func(ctx context.Context, mid billing.MerchantID, key string) error {
		return approveSolanaSigner(ctx, application, declaration, mid, key)
	}
	if signerPending {
		confirmSigner(application, declaration)
	}
	rt.CheckBookIdentity(ctx)
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
	for _, key := range slices.Sorted(maps.Keys(cfg.Merchant.PSPs)) {
		if err := config.ValidatePSPKeys(key, cfg.Merchant.PSPs[key]); err != nil {
			return fmt.Errorf("openrails: Config.Merchant.%w", err)
		}
	}
	cfg.Database.RiverSchema = strings.ToLower(strings.TrimSpace(cfg.Database.RiverSchema))
	if err := validRiverSchema(config.RiverSchemaName(cfg)); err != nil {
		return err
	}
	if err := config.ValidateCheckout(cfg.Checkout); err != nil {
		return fmt.Errorf("openrails: Config.Checkout: %w", err)
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
	if deps.Email != nil && cfg.SendGrid != nil {
		return fmt.Errorf("openrails: set Deps.Email or Config.SendGrid, not both")
	}
	if cfg.SendGrid != nil && strings.TrimSpace(cfg.SendGrid.APIKey) == "" {
		return fmt.Errorf("openrails: Config.SendGrid.APIKey is required")
	}
	if !cfg.RateLimitsDisabled {
		if cfg.RateLimits == nil {
			cfg.RateLimits = config.DefaultRateLimits()
		}
		if cfg.Captcha == nil {
			cfg.Captcha = config.DefaultCaptcha()
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

// validRiverSchema accepts what River and riverhelpers accept: an identifier
// short enough for River's own derived names.
func validRiverSchema(schema string) error {
	if !validIdentifier(schema) || len(schema) > 63-len(".river_leadership") {
		return fmt.Errorf("openrails: River schema %q is not a valid schema name (at most 46 characters); set Config.Database.RiverSchema", schema)
	}
	return nil
}

// ConfiguredMerchant is the declared merchant's ID, zero when unbound.
func (e *Engine) ConfiguredMerchant() billing.MerchantID { return e.App.Runtime.ConfiguredMerchant() }

// Start starts OpenRails' workers on goroutines Close stops: River, the loops
// that run outside it and the progress monitor. fleet nil runs OpenRails' own
// River client; otherwise fleet is the host's, composed with RiverJobs, and
// the host starts and stops it. It runs no DDL, and ctx ending stops nothing:
// only Close does.
func (e *Engine) Start(ctx context.Context, fleet *river.Client[pgx.Tx]) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return fmt.Errorf("openrails: client is closed")
	}
	if e.stopWorkers != nil {
		return fmt.Errorf("openrails: already started")
	}
	wctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop, err := e.App.Runtime.StartWorkers(wctx, fleet)
	if err != nil {
		cancel()
		return fmt.Errorf("openrails: %w", err)
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
