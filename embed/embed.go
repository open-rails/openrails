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
	"github.com/open-rails/openrails/internal/db"
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
// standalone gin handler (pkg/embedded/gin.Handler over Runtime.Embedded())
// mounts /v1/self/* + /v1/merchant-admin/* authenticated by it — no control
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
//				Permissions: []string{"openrails:self:billing:read"},
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

	// Merchant binds this embedded engine to a single tenant at construction
	// (#336): doujins/hentai0 set it to their 'doujins' tenant SLUG. There is
	// no default tenant — leave it empty only if you resolve a tenant per
	// request some other way, otherwise merchant-owned operations hard-fail. It
	// is propagated to Config.Merchant in New; the slug is resolved to the
	// internal tenant id once at bootstrap so the HTTP resolver and the
	// Solana-secret seed pin it. To serve another tenant, construct another
	// engine/client.
	Merchant string
}

// HandlerOptions selects the HTTP route groups for Runtime.Handler. It is
// pkg/embedded.HTTPHandlerOptions; zero value defaults to user+admin+webhooks.
type HandlerOptions = embedded.HTTPHandlerOptions

// Runtime is the in-process OpenRails engine plus its SDK adapter. It is the
// ONE entry point an embedding host needs: Client() for the unified interface,
// Handler() to mount the embedded HTTP surface, RunWorkers/Close for lifecycle.
type Runtime struct {
	emb *embedded.Embedded
	svc *service.Service

	// tenantID is the engine's construction-time bound tenant (#336/#478),
	// resolved once from Config.Merchant. The unified Client adapter injects it
	// onto each call's context so an embedded host can call the service-token-
	// equivalent surface with a plain context (the host IS the principal and is
	// trusted for its own tenant); the standalone/gin surface still resolves the
	// tenant from the request/credential instead. Zero when no tenant is bound.
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
	// Bind the engine to its construction-time tenant (#336): propagate the
	// slug to Config.Merchant so the HTTP resolver and merchant-owned boot work pin
	// it (resolved slug→id once at bootstrap, downstream).
	if opts.Merchant != "" {
		opts.Config.Merchant = opts.Merchant
	}
	if opts.RunMigrations {
		if opts.Config.DB == nil || opts.Config.DB.URL == "" {
			return nil, fmt.Errorf("openrails embed: RunMigrations requires config.DB.URL (or run migrations yourself via cmd/openrails migrate)")
		}
		if err := migrate.RunPostgres(ctx, opts.Config); err != nil {
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
	// Idempotently REGISTER the bound merchant (a billing bucket) from config
	// (#480, supersedes #478). A host binds the engine to a merchant SLUG
	// (opts.Merchant), but nothing else creates that row; without it, merchant
	// resolution (HTTP handler / self handler) would fail. Embedded OpenRails runs
	// NO AuthKit and stores ZERO auth — RegisterMerchant carries billing-only
	// state (NO issuer/JWKS). The host owns the embed + DB, so registration is
	// safe and idempotent. Runs after migrations so the merchants table exists; a
	// second New is an idempotent no-op.
	var boundMerchant merchant.ID
	if opts.Config.Merchant != "" {
		if a := emb.App(); a != nil && a.Runtime != nil && a.Runtime.DB != nil {
			ectx := ctx
			id, rerr := db.RegisterMerchant(ectx, a.Runtime.DB.Qx(ectx), db.RegisterMerchantOptions{
				Slug: opts.Config.Merchant,
			})
			if rerr != nil {
				_ = emb.Close(ctx)
				return nil, fmt.Errorf("openrails embed: %w", rerr)
			}
			boundMerchant = id
		}
	}
	svc, err := emb.Service()
	if err != nil {
		_ = emb.Close(ctx)
		return nil, err
	}

	r := &Runtime{emb: emb, svc: svc, tenantID: boundMerchant}
	if opts.RunWorkers {
		wctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		r.workersCancel = cancel
		r.workersDone = make(chan error, 1)
		go func() { r.workersDone <- emb.RunWorkers(wctx) }()
	}
	return r, nil
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
// passthrough to pkg/embedded.NewHTTPHandler. The service-token-authenticated
// /v1/service/* surface is part of the standalone server (pkg/embedded/gin
// Handler / internal/http); an embedded host normally uses Client() instead.
func (r *Runtime) Handler(opts HandlerOptions) http.Handler {
	return r.emb.NewHTTPHandler(opts)
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
