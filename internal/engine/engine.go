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
	"maps"
	"slices"

	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/routebundle"
	"github.com/open-rails/openrails/internal/service"
)

// Engine is one embedded engine.
type Engine struct {
	App *app.App
	svc *service.Service

	// declared are Config.Merchants' ids, by slug.
	declared map[string]billing.MerchantID

	mu          sync.Mutex
	closed      bool
	stopWorkers func()
	routes      map[string][]routebundle.Route

	handlerOnce sync.Once
	handler     http.Handler

	closeOnce sync.Once
	closeErr  error
}

// Of returns the engine behind an openrails.Engine or one of its handles, nil
// for a remote handle. The root package sets it; operator tooling inside the
// module uses it.
var Of func(client any) *Engine

// Graph returns the application graph behind an openrails.Engine or one of
// its handles.
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
// database; nothing else does. Only Postgres fails construction: Vault login,
// PSP posture and Redis recover in the background (see Ready and Probes). It
// never changes a catalog: a catalog document applies by an explicit
// ApplyCatalog.
func New(ctx context.Context, cfg config.Config, deps config.Deps) (*Engine, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	declarations, err := validate(&cfg, deps)
	if err != nil {
		return nil, err
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
		FXTransport:     deps.FXTransport,
		Clock:           deps.Clock,
		EmailSender:     deps.Email,
		UserInfo:        deps.UserInfo,
		Vault:           deps.Vault,
	}
	application, err := app.BootstrapWithOptions(ctx, &cfg, bootstrap)
	if err != nil {
		return nil, fmt.Errorf("bootstrap application: %w", err)
	}
	e := &Engine{App: application, declared: map[string]billing.MerchantID{}}
	fail := func(err error) (*Engine, error) {
		_ = e.Close(ctx)
		return nil, err
	}
	rt := application.Runtime
	// Client operations need the same provider and secret graph as the
	// standalone server; neither workers nor routes are prerequisites.
	if err := rt.EnsureMerchantsService(ctx); err != nil {
		return fail(fmt.Errorf("initialize merchant services: %w", err))
	}
	rt.Merchants.WithFakeProviders(seams(deps)...)
	// Each declared merchant is provisioned before a route or a worker can
	// see it; the issuer that binds its credentials is recorded with it.
	ids := make([]billing.MerchantID, len(declarations))
	issuers := map[string]billing.MerchantID{}
	bySlug := map[billing.MerchantID]config.MerchantDeclaration{}
	var signersPending []config.MerchantDeclaration
	for i, m := range declarations {
		id, pending, err := upsertMerchantConfig(ctx, application, m, true)
		if err != nil {
			return fail(err)
		}
		ids[i], e.declared[m.Slug], bySlug[id] = id, id, m
		if m.Issuer != "" {
			issuers[m.Issuer] = id
		}
		if pending {
			signersPending = append(signersPending, m)
		}
	}
	rt.DeclareMerchants(ids, issuers)
	if _, err := rt.GetBillingPeriodicJobs(ctx); err != nil {
		return fail(fmt.Errorf("build billing periodic jobs: %w", err))
	}
	if e.svc, err = service.New(rt); err != nil {
		return fail(err)
	}
	rt.ApproveSolanaSigner = func(ctx context.Context, mid billing.MerchantID, key string) error {
		return approveSolanaSigner(ctx, application, bySlug[mid], mid, key)
	}
	for _, m := range signersPending {
		confirmSigner(application, m)
	}
	rt.CheckBookIdentity(ctx)
	rt.PreloadMerchantConfig(ids...)
	rt.StartProviderPosture(ids...)
	return e, nil
}

// validate enforces what embedded construction must declare and seeds the
// protective defaults the standalone loader applies; it returns
// Config.Merchants in slug order. Every refusal happens before any resource
// opens.
func validate(cfg *config.Config, deps config.Deps) ([]config.MerchantDeclaration, error) {
	// Unset would run fail-closed readonly: renewals, retries and refunds
	// would silently never reach a provider. The host states it.
	switch mode := strings.ToLower(strings.TrimSpace(cfg.ProviderWriteMode)); mode {
	case config.ProviderWriteModeFull, config.ProviderWriteModeLimited, config.ProviderWriteModeReadOnly:
	case "":
		return nil, fmt.Errorf("openrails: Config.ProviderWriteMode is required; set full, limited or readonly explicitly (readonly never charges: renewals, retries and refunds wait)")
	default:
		return nil, fmt.Errorf("openrails: Config.ProviderWriteMode %q is invalid; use full, limited or readonly", cfg.ProviderWriteMode)
	}
	declarations, err := config.DeclaredMerchants(cfg)
	if err != nil {
		return nil, fmt.Errorf("openrails: %w", err)
	}
	cfg.Database.RiverSchema = strings.ToLower(strings.TrimSpace(cfg.Database.RiverSchema))
	if err := validRiverSchema(config.RiverSchemaName(cfg)); err != nil {
		return nil, err
	}
	if live := declaredLive(declarations); live != "" {
		if fakes := fakeProviders(cfg, deps); len(fakes) > 0 {
			return nil, fmt.Errorf("openrails: %s is a test seam and is refused beside a live PSP (%s)", strings.Join(fakes, ", "), live)
		}
	}
	if deps.Email != nil && cfg.SMTP != nil {
		return nil, fmt.Errorf("openrails: set Deps.Email or Config.SMTP, not both")
	}
	if cfg.SMTP != nil && strings.TrimSpace(cfg.SMTP.Host) == "" {
		return nil, fmt.Errorf("openrails: Config.SMTP.Host is required")
	}
	if !cfg.RateLimitsDisabled {
		if cfg.RateLimits == nil {
			cfg.RateLimits = config.DefaultRateLimits()
		}
		if cfg.Captcha == nil {
			cfg.Captcha = config.DefaultCaptcha()
		}
	}
	return declarations, nil
}

// fakeProviders names the test seams and fake providers cfg and deps wire:
// only a deployment with no live PSP may use them.
func fakeProviders(cfg *config.Config, deps config.Deps) []string {
	out := append(config.FakeProviders(cfg), seams(deps)...)
	slices.Sort(out)
	return out
}

// seams names the Deps test seams wired.
func seams(deps config.Deps) []string {
	var out []string
	for name, set := range map[string]bool{
		"Deps.StripeTransport": deps.StripeTransport != nil, "Deps.NMITransport": deps.NMITransport != nil, "Deps.FXTransport": deps.FXTransport != nil,
		"Deps.Clock": deps.Clock != nil,
	} {
		if set {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// declaredLive names a live PSP the declarations declare, or "".
func declaredLive(declarations []config.MerchantDeclaration) string {
	environments := config.DeclaredEnvironments(declarations)
	for _, name := range slices.Sorted(maps.Keys(environments)) {
		if config.LiveEnvironment(environments[name]) {
			return name
		}
	}
	return ""
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

// DeclaredMerchant is the merchant Config declares under slug.
func (e *Engine) DeclaredMerchant(slug string) (billing.MerchantID, bool) {
	id, ok := e.declared[billing.NormalizeMerchantSlug(slug)]
	return id, ok && !id.IsZero()
}

// Start starts OpenRails' workers on goroutines Close stops: River, the loops
// that run outside it and the progress monitor. fleet nil runs OpenRails' own
// River client; otherwise fleet is the host's, composed with RiverJobs, and
// the host starts and stops it. It runs no DDL, and ctx ending stops nothing:
// only Close does.
func (e *Engine) Start(ctx context.Context, fleet *river.Client[pgx.Tx]) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return fmt.Errorf("openrails: engine is closed")
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
