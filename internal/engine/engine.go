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
	"github.com/open-rails/openrails/internal/retry"
	"github.com/open-rails/openrails/internal/service"
)

// Engine is one embedded engine.
type Engine struct {
	App *app.App
	svc *service.Service

	// declMu guards declared, issuers and bySlug: a merchant whose
	// provisioning failed at New joins them once a background retry succeeds.
	declMu sync.RWMutex
	// declared are Config.Merchants' provisioned ids, by slug.
	declared       map[string]billing.MerchantID
	issuers        map[string]billing.MerchantID
	bySlug         map[billing.MerchantID]config.MerchantDeclaration
	merchantGroups billing.MerchantGroupReader

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
// AuthKit migrates itself as it is built.
// Database.SkipMigrations validates existing schemas instead of
// applying startup schema, partition or River changes. ctx bounds the
// database; nothing else does. Only the database, an unknown provider write
// mode, an invalid River schema and money-safety refusals (live beside
// non-live PSPs, a test seam beside a live PSP) fail construction. Anything
// else that cannot be served is refused alone and reported (Ready, Probes'
// openrails_configuration): an optional setting, a declared merchant, one of
// its documents. Vault login, PSP posture and Redis recover in the
// background. It never changes a catalog: a catalog document applies by an
// explicit ApplyCatalog.
func New(ctx context.Context, cfg config.Config, deps config.Deps) (*Engine, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	declarations, refused, err := validate(&cfg, deps)
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
	e := &Engine{App: application, merchantGroups: deps.MerchantGroups, declared: map[string]billing.MerchantID{}, issuers: map[string]billing.MerchantID{}, bySlug: map[billing.MerchantID]config.MerchantDeclaration{}}
	fail := func(err error) (*Engine, error) {
		_ = e.Close(ctx)
		return nil, err
	}
	rt := application.Runtime
	rt.RefuseDegraded(refused)
	// Client operations need the same provider and secret graph as the
	// standalone server; neither workers nor routes are prerequisites.
	if err := rt.ServeMerchantsService(ctx); err != nil {
		return fail(fmt.Errorf("initialize merchant services: %w", err))
	}
	rt.Merchants.WithFakeProviders(seams(deps)...)
	// Each declared merchant is provisioned before a route or a worker can
	// see it; the issuer that binds its credentials is recorded with it. One
	// that cannot be is refused alone and retried in the background.
	for _, m := range declarations {
		id, err := upsertMerchantConfig(ctx, application, m, e.merchantGroups)
		if err != nil {
			e.provisionLater(m, err)
			continue
		}
		e.admit(m, id, false)
	}
	e.publishDeclared()
	if err := rt.ValidateBillingPeriodicJobs(ctx); err != nil {
		return fail(fmt.Errorf("build billing periodic jobs: %w", err))
	}
	if e.svc, err = service.New(rt); err != nil {
		return fail(err)
	}
	rt.CheckBookIdentity(ctx)
	ids := e.declaredIDs()
	rt.PreloadMerchantConfig(ids...)
	rt.StartProviderPosture(ids...)
	return e, nil
}

// admit records a provisioned declared merchant. late is a merchant a
// background retry provisioned after New: it is published and verified at
// once.
func (e *Engine) admit(m config.MerchantDeclaration, id billing.MerchantID, late bool) {
	rt := e.App.Runtime
	e.declMu.Lock()
	e.declared[m.Slug], e.bySlug[id] = id, m
	if m.Issuer != "" {
		e.issuers[m.Issuer] = id
	}
	e.declMu.Unlock()
	rt.NameMerchant(id, m.Slug)
	if late {
		e.publishDeclared()
		rt.Unrefuse(merchantSubject(m.Slug))
		rt.PreloadMerchantConfig(id)
		rt.StartProviderPosture(e.declaredIDs()...)
	}
}

// provisionLater refuses a declared merchant whose provisioning failed and
// retries it in the background with capped backoff until it succeeds.
func (e *Engine) provisionLater(m config.MerchantDeclaration, err error) {
	rt := e.App.Runtime
	rt.Refuse(merchantSubject(m.Slug), fmt.Errorf("%w; the merchant is not provisioned, retrying in the background", err))
	rt.Go("provision merchant "+m.Slug, func(ctx context.Context) {
		_ = retry.Forever(ctx, func(ctx context.Context) error {
			id, err := upsertMerchantConfig(ctx, e.App, m, e.merchantGroups)
			if err != nil {
				rt.Refuse(merchantSubject(m.Slug), fmt.Errorf("%w; the merchant is not provisioned, retrying in the background", err))
				return err
			}
			e.admit(m, id, true)
			return nil
		}, nil)
	})
}

func merchantSubject(slug string) string { return "merchants." + slug }

// publishDeclared hands the runtime the provisioned merchants and the
// issuers bound to them.
func (e *Engine) publishDeclared() {
	e.declMu.RLock()
	issuers := maps.Clone(e.issuers)
	e.declMu.RUnlock()
	e.App.Runtime.DeclareMerchants(e.declaredIDs(), issuers)
}

// declaredIDs are the provisioned declared merchants, in slug order.
func (e *Engine) declaredIDs() []billing.MerchantID {
	e.declMu.RLock()
	defer e.declMu.RUnlock()
	ids := make([]billing.MerchantID, 0, len(e.declared))
	for _, slug := range slices.Sorted(maps.Keys(e.declared)) {
		ids = append(ids, e.declared[slug])
	}
	return ids
}

// validate enforces what embedded construction must declare and seeds the
// protective defaults the standalone loader applies; it returns
// Config.Merchants in slug order, and the entries it refused alone. Every
// fatal refusal happens before any resource opens.
func validate(cfg *config.Config, deps config.Deps) ([]config.MerchantDeclaration, []config.Degradation, error) {
	// Unset would run fail-closed readonly: renewals, retries and refunds
	// would silently never reach a provider. The host states it.
	switch mode := strings.ToLower(strings.TrimSpace(cfg.ProviderWriteMode)); mode {
	case config.ProviderWriteModeFull, config.ProviderWriteModeLimited, config.ProviderWriteModeReadOnly:
	case "":
		return nil, nil, fmt.Errorf("openrails: Config.ProviderWriteMode is required; set full, limited or readonly explicitly (readonly never charges: renewals, retries and refunds wait)")
	default:
		return nil, nil, fmt.Errorf("openrails: Config.ProviderWriteMode %q is invalid; use full, limited or readonly", cfg.ProviderWriteMode)
	}
	declarations, refused, err := config.DeclaredMerchants(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("openrails: %w", err)
	}
	cfg.Database.RiverSchema = strings.ToLower(strings.TrimSpace(cfg.Database.RiverSchema))
	if err := validRiverSchema(config.RiverSchemaName(cfg)); err != nil {
		return nil, nil, err
	}
	if live := declaredLive(declarations); live != "" {
		if fakes := fakeProviders(cfg, deps); len(fakes) > 0 {
			return nil, nil, fmt.Errorf("openrails: %s is a test seam and is refused beside a live PSP (%s)", strings.Join(fakes, ", "), live)
		}
	}
	// Vault holds a merchant's configuration: what its declaration names
	// beside its slug is ignored, and reported.
	for i, m := range declarations {
		if err := config.RefuseDeclarationBesideVault(cfg, m.Slug, m); err != nil {
			refused = append(refused, config.Degradation{Field: merchantSubject(m.Slug) + ".declaration", Err: fmt.Errorf("%w; Vault's configuration serves", err)})
			declarations[i] = config.MerchantDeclaration{Slug: m.Slug, Issuer: m.Issuer}
		}
	}
	if !cfg.RateLimitsDisabled {
		if cfg.RateLimits == nil {
			cfg.RateLimits = config.DefaultRateLimits()
		}
		if cfg.Captcha == nil {
			cfg.Captcha = config.DefaultCaptcha()
		}
	}
	return declarations, refused, nil
}

// fakeProviders names the test seams and fake providers cfg and deps wire:
// no live PSP is declared beside them.
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
	for _, m := range declarations {
		for _, key := range slices.Sorted(maps.Keys(m.PSPs)) {
			if p := m.PSPs[key]; config.LiveEnvironment(string(p.Rail), p.Environment) {
				return m.Slug + "/" + strings.ToLower(strings.TrimSpace(key))
			}
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

// DeclaredMerchant is the merchant Config declares under slug, once
// provisioned.
func (e *Engine) DeclaredMerchant(slug string) (billing.MerchantID, bool) {
	e.declMu.RLock()
	defer e.declMu.RUnlock()
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
