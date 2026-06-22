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
	boot "github.com/open-rails/openrails/internal/bootstrap"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/migrate"
	"github.com/open-rails/openrails/pkg/embedded"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/open-rails/openrails/pkg/service"
)

// Options configures the embedded runtime. It wraps pkg/embedded.Options
// (Config, PGXPool, Redis, Authenticator, DelegatedAuthenticator, Cache) and
// adds lifecycle switches.
//
// DelegatedAuthenticator (issue #339) is the host-pluggable identity seam for
// the browser-direct self-service surface: a host that verifies its own
// credentials supplies a billingauth.DelegatedAuthenticator returning the
// explicitly mapped {tenant, subject, permissions} principal, and the
// standalone gin handler (pkg/embedded/gin.StandaloneHandler over Runtime.Embedded())
// mounts /v1/me/* + /v1/orgs/* authenticated by it — no control
// plane required. For example:
//
//	opts.DelegatedAuthenticator = billingauth.DelegatedAuthenticatorFunc(
//		func(ctx context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
//			user, err := hostAuth.Verify(r) // the host's own credential check
//			if err != nil {
//				return nil, billingauth.ErrUnauthenticated
//			}
//			return &billingauth.DelegatedPrincipal{
//				MerchantID:    hostCfg.OpenRailsTenantID, // explicit per-deployment mapping to a real tenant
//				SubjectID:   user.CanonicalID,
//				Permissions: []string{"self:billing:read"},
//			}, nil
//		})
type Options struct {
	embedded.Options

	// RunMigrations applies the OpenRails Postgres migrations (and the
	// ClickHouse migrations when Options.Config.ClickHouse is set) before the
	// app graph is built. It requires Options.Config.DB.URL — the migration
	// runner opens its own connection; a host that supplies only a PGXPool must
	// either also set DB.URL or run migrations itself (internal/migrate is what
	// cmd/openrails's `migrate` entrypoint calls).
	RunMigrations bool

	// RunWorkers starts the River background workers on a goroutine owned by
	// the Runtime (stopped by Close). The worker context is detached from the
	// ctx passed to New (context.WithoutCancel) so a short-lived startup
	// context does not kill long-running workers; cancellation is Close's job.
	// Leave false to drive workers yourself via Runtime.RunWorkers.
	RunWorkers bool

	// Merchant binds this embedded engine to a single tenant at construction:
	// doujins/hentai0 set it to their merchant slug. It is resolved to an
	// internal merchant id during New and stored as construction state, not
	// loaded from config.yaml/.env. To serve another tenant, construct another
	// engine/client.
	Merchant string

	// MerchantSettings seeds merchant-owned settings for the bound Merchant at
	// startup, mirroring /v1/merchant/settings for embedded hosts.
	MerchantSettings openrails.MerchantSettings
}

// HandlerOptions selects the HTTP route groups for Runtime.Handler. It is
// pkg/embedded.HTTPHandlerOptions; zero value uses EmbeddedDefaultRouteSets.
type HandlerOptions = embedded.HTTPHandlerOptions

// RouteSet names a mountable billing HTTP route group.
type RouteSet = embedded.RouteSet

const (
	RouteSetCheckout         = embedded.RouteSetCheckout
	RouteSetCustomer         = embedded.RouteSetCustomer
	RouteSetMerchantAdmin    = embedded.RouteSetMerchantAdmin
	RouteSetMerchantSettings = embedded.RouteSetMerchantSettings
	RouteSetMerchantAPI      = embedded.RouteSetMerchantAPI
	RouteSetWebhooks         = embedded.RouteSetWebhooks
)

var (
	EmbeddedDefaultRouteSets   = append([]RouteSet(nil), embedded.EmbeddedDefaultRouteSets...)
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

	// tenantID is the engine's construction-time bound tenant, resolved once from
	// Options.Merchant. The unified Client adapter injects it onto each call's
	// context so an embedded host can call the API-key-equivalent surface
	// with a plain context. Zero when no tenant is bound.
	tenantID merchant.ID

	workersCancel context.CancelFunc
	workersDone   chan error
}

// New builds the embedded runtime: optional migrations, then the gin-free app
// graph (pkg/embedded.New), then the service facade the Client adapts.
func New(ctx context.Context, opts Options) (*Runtime, error) {
	if opts.Config == nil {
		return nil, fmt.Errorf("openrails embed: config is required")
	}
	if opts.RunMigrations {
		if opts.Config.DB == nil || opts.Config.DB.URL == "" {
			return nil, fmt.Errorf("openrails embed: RunMigrations requires config.DB.URL (or run migrations yourself via cmd/openrails migrate)")
		}
		// Embedded skips the AuthKit (`profiles`) migrations (#544): the host owns
		// auth, so OpenRails creates only its own portable schema + River (`public`).
		if err := migrate.RunPostgresEmbedded(ctx, opts.Config); err != nil {
			return nil, fmt.Errorf("openrails embed: run postgres migrations: %w", err)
		}
		if opts.Config.ClickHouse != nil {
			if err := migrate.RunClickHouse(ctx, opts.Config); err != nil {
				return nil, fmt.Errorf("openrails embed: run clickhouse migrations: %w", err)
			}
		}
	}

	emb, err := embedded.New(opts.Options)
	if err != nil {
		return nil, err
	}
	// Idempotently provision the bound merchant (a billing bucket) from the
	// explicit embedded construction option through the same #527 merchant
	// provisioning boundary used by standalone manifests. Embedded OpenRails runs
	// NO AuthKit here and creates no AuthKit objects; the merchant's backing org is
	// the host's AuthKit org of the SAME slug (#541 — merchant slug == org slug,
	// 1:1), so OpenRails neither creates nor records it (owner_org_id stays NULL in
	// embedded, set only in standalone where OpenRails owns the org).
	var boundMerchant merchant.ID
	if opts.Merchant != "" {
		if a := emb.App(); a != nil && a.Runtime != nil && a.Runtime.DB != nil {
			tn, rerr := boot.ProvisionMerchant(ctx, boot.ProvisionMerchantRequest{
				Config:   opts.Config,
				Database: a.Runtime.DB,
				Merchant: embeddedManifestMerchant(opts.Merchant, opts.MerchantSettings),
				Options:  boot.MerchantManifestReconcileOptions{Insert: true},
			})
			if rerr != nil {
				_ = emb.Close(ctx)
				return nil, fmt.Errorf("openrails embed: %w", rerr)
			}
			boundMerchant = tn.ID
			a.Runtime.ConfiguredMerchant = boundMerchant
		}
	}
	svc, err := emb.Service()
	if err != nil {
		_ = emb.Close(ctx)
		return nil, err
	}

	r := &Runtime{emb: emb, svc: svc, tenantID: boundMerchant}
	if hasMerchantSettings(opts.MerchantSettings) {
		if boundMerchant.IsZero() {
			_ = emb.Close(ctx)
			return nil, fmt.Errorf("openrails embed: MerchantSettings requires Merchant")
		}
		if err := r.Client().SetMerchantSettings(ctx, startupMerchantSettings(opts.Merchant, opts.MerchantSettings)); err != nil {
			_ = emb.Close(ctx)
			return nil, fmt.Errorf("openrails embed: seed merchant settings: %w", err)
		}
	}
	if opts.RunWorkers {
		wctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		r.workersCancel = cancel
		r.workersDone = make(chan error, 1)
		go func() { r.workersDone <- emb.RunWorkers(wctx) }()
	}
	return r, nil
}

func hasMerchantSettings(in openrails.MerchantSettings) bool {
	return in.Profile != nil ||
		len(in.TierSchedules) > 0 ||
		len(in.TierSpendLimits) > 0 ||
		len(in.DelegatedInvokerWastedSpendLimits) > 0
}

func startupMerchantSettings(merchantSlug string, in openrails.MerchantSettings) openrails.MerchantSettings {
	if in.Profile != nil && in.Profile.DisplayName == "" {
		profile := *in.Profile
		profile.DisplayName = merchantSlug
		in.Profile = &profile
	}
	return in
}

func embeddedManifestMerchant(merchantSlug string, cfg openrails.MerchantSettings) boot.ManifestMerchant {
	displayName := merchantSlug
	profile := boot.ManifestMerchantProfile{}
	if cfg.Profile != nil {
		displayName = cfg.Profile.DisplayName
		profile = boot.ManifestMerchantProfile{
			DisplayName: cfg.Profile.DisplayName,
			LogoURL:     cfg.Profile.LogoURL,
			FromEmail:   cfg.Profile.FromEmail,
			SupportURL:  cfg.Profile.SupportURL,
		}
	}
	if displayName == "" {
		displayName = merchantSlug
	}
	return boot.ManifestMerchant{
		Slug:        merchantSlug,
		DisplayName: displayName,
		Profile:     profile,
	}
}

// Client returns the openrails.Client adapter over the in-process engine. Each
// method transcribes the wire→service mapping of the corresponding standalone
// handler (internal/http/handlers/service_*.go), including the error→status
// mapping, so it is observably identical to NewRemote against the same engine
// (enforced by conformance_integration_test.go).
func (r *Runtime) Client(opts ...ClientOption) openrails.Client {
	c := &localClient{svc: r.svc, tenantID: r.tenantID}
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
// passthrough to pkg/embedded.NewHTTPHandler. The service-credential-authenticated
// /billing/v1/service/* surface is opt-in via embedded.RouteSetMerchantAPI; an
// embedded host normally uses Client() instead.
func (r *Runtime) Handler(opts HandlerOptions) http.Handler {
	asm := embedhttp.FromApp(r.emb.App())
	asm.ConfiguredMerchant = r.tenantID
	return asm.NewHTTPHandler(embedhttp.Options{
		RouteSets: opts.RouteSets,
	})
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
