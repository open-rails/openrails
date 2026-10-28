package openrails

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/http/inprocess"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

func init() {
	engine.Of = func(v any) *engine.Engine {
		switch v := v.(type) {
		case *Engine:
			if v != nil {
				return v.engine
			}
		case *LocalClient:
			if v != nil && v.Client != nil {
				return v.engine
			}
		case *Client:
			if v != nil {
				return v.engine
			}
		}
		return nil
	}
	engine.NewClient = func(transport http.RoundTripper, merchant billing.MerchantID) any {
		return &Client{inprocess: true, baseURL: engine.InprocessBaseURL, client: &http.Client{Transport: transport}, merchantID: merchant}
	}
}

// Engine is OpenRails running in this process: the pool, the workers, the
// routes and every merchant in its directory. It carries what spans
// merchants; Merchant returns the handle each merchant's operations are on.
type Engine struct {
	engine *engine.Engine
	// client calls the engine over its in-process transport; every handle
	// shares it.
	client *http.Client
	// reads are the handles' cached reads, dropped for a merchant as soon as
	// the engine commits a change to it.
	reads *reads
}

// New runs the OpenRails engine in this process. Before anything else touches
// the database it creates or upgrades OpenRails' tables in
// Config.Database.Schema, River's in Config.Database.RiverSchema and this
// month's partitions; the pool's role owns them. That is idempotent, replicas
// booting together take turns on an advisory lock, and a schema a newer build
// already migrated is left as it is. It starts no workers: Start does, and
// jobs queued before it wait in Config.Database.RiverSchema.
// With Database.SkipMigrations, New validates existing
// migration ledgers instead; run Migrate explicitly before starting the app.
// Normal runtime partition maintenance remains active. ctx bounds the
// wait for the database; nothing else does. It provisions each merchant
// Config.Merchants declares and never changes a catalog: a catalog document
// is an operator's ApplyCatalog. Vault login, PSP posture checks and Redis
// recover in the background and fail only the features that need them (see
// Probes). It fails only on what the engine cannot serve without: the
// database and its migrations, an unknown ProviderWriteMode, an invalid
// River schema, live PSPs beside non-live ones or beside a test seam. Any
// other setting, merchant or document that cannot be served is refused alone
// and reported by Ready and the openrails_configuration probe.
func New(ctx context.Context, cfg Config, deps Deps) (*Engine, error) {
	inner, err := engine.New(ctx, cfg, deps)
	if err != nil {
		return nil, err
	}
	e := &Engine{engine: inner, client: &http.Client{Transport: inner.Transport()}}
	var refused []config.Degradation
	if e.reads, refused, err = newReads(inProcess, cfg.ReadCache, 0, e.handle); err != nil {
		return nil, errors.Join(err, inner.Close(ctx))
	}
	inner.App.Runtime.RefuseDegraded(refused)
	inner.App.Runtime.OnMerchantChange(e.reads.forget)
	return e, nil
}

func (e *Engine) handle(id billing.MerchantID) *Client {
	return &Client{engine: e.engine, inprocess: true, baseURL: engine.InprocessBaseURL, client: e.client, merchantID: id, reads: e.reads}
}

// Merchant is the handle of merchant id: every merchant-scoped operation,
// acting as the host itself, with no credential. Holding the Engine is the
// authority, and the binding never crosses a wire. It does no I/O and
// refuses only the zero ID: each call reads the merchant from the directory,
// so a merchant provisioned after New is served at once, and a call for an
// unknown, deleted or archived one fails with billing.ErrMerchantUnresolved.
// The caller's context bounds each call.
func (e *Engine) Merchant(id billing.MerchantID) (*Client, error) {
	if id.IsZero() {
		return nil, invalidErr("merchant id is required")
	}
	return e.handle(id), nil
}

// MerchantBySlug is Merchant for the merchant Config.Merchants declares
// under slug; any other slug is billing.ErrMerchantUnresolved. It does no
// I/O.
func (e *Engine) MerchantBySlug(slug string) (*Client, error) {
	id, ok := e.engine.DeclaredMerchant(slug)
	if !ok {
		return nil, fmt.Errorf("%w: Config declares no merchant %q", billing.ErrMerchantUnresolved, slug)
	}
	return e.Merchant(id)
}

// WithInvoker names the signed-in person an in-process call acts for, by
// issuer and subject as the host's identity provider verified them. The call
// is then theirs: their destructive-action ceilings and holds apply, as to
// their own HTTP call. A call naming no one is the host's own work: recorded
// in the destructive-action audit, but never counted or held. An issuer
// beginning "openrails:" or an empty name is refused (host_principal_invalid).
// A remote handle ignores it; its credential names the caller.
func WithInvoker(ctx context.Context, invoker Invoker) context.Context {
	return inprocess.WithInvoker(ctx, invoker)
}

// StartOption adjusts Start.
type StartOption func(*startOptions)

type startOptions struct {
	fleet    *river.Client[pgx.Tx]
	fleetSet bool
}

// WithRiverClient runs OpenRails' jobs on the host's River fleet, which
// riverhelpers.New built with RiverJobs in Config.Database.RiverSchema
// (alongside the host's and AuthKit's jobs). OpenRails enqueues through it and never starts
// or stops it.
func WithRiverClient(fleet *river.Client[pgx.Tx]) StartOption {
	return func(o *startOptions) { o.fleet, o.fleetSet = fleet, true }
}

// Start starts OpenRails' background work: River (renewals, dunning,
// invoices, provider intents), the Solana Pay poller and the job-progress
// monitor. With no options it builds and runs OpenRails' own River client in
// Config.Database.RiverSchema; with WithRiverClient the jobs run on the host's
// fleet. Jobs queued before Start wait for it. It runs no DDL, and ctx ending stops
// nothing: Close stops what Start started. Call it once, before serving.
func (e *Engine) Start(ctx context.Context, opts ...StartOption) error {
	var o startOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	if o.fleetSet && o.fleet == nil {
		return fmt.Errorf("openrails: WithRiverClient requires a River client")
	}
	return e.engine.Start(ctx, o.fleet)
}

// Close stops what Start started (OpenRails' own River, never the host's
// fleet) and closes the engine, leaving the host's pool, Redis and Vault
// clients open. Its handles answer nothing afterwards.
func (e *Engine) Close(ctx context.Context) error {
	if e == nil {
		return nil
	}
	e.reads.close()
	return e.engine.Close(ctx)
}

// Ready reports whether the engine can serve: Postgres, the merchant
// directory and River (the host's fleet bound to RiverJobs, or OpenRails' own
// running since Start); optional providers never fail it (see Probes).
func (e *Engine) Ready(ctx context.Context) error { return e.engine.Ready(ctx) }

// Probe checks one optional dependency.
type Probe struct {
	// Name is stable for dashboards: openrails_vault, openrails_psp_posture,
	// openrails_configuration, openrails_solana_crankers,
	// openrails_job_progress.
	Name string
	// Check returns nil while the dependency is usable; it honors ctx.
	Check func(context.Context) error
}

// Probes lists the optional dependencies the engine reconnects to in the
// background, for the host's dependency supervisor. openrails_vault is a
// live token lookup (only when OpenRails logs in to Vault itself).
// openrails_psp_posture fails while a PSP is unverified or disarmed.
// openrails_configuration fails while something configured is not served,
// naming each with why: an optional setting turned off, a declared merchant
// not provisioned yet, a merchant document refused (a PSP whose secret or
// declaration does not check). openrails_solana_crankers fails while a listed
// cranker cannot sign, or a plan with subscribers is owned by no listed
// cranker: its renewals are not pulled. openrails_job_progress, a read of
// River's tables, fails while the fleet's River schema has no leader, an
// OpenRails job runs past its timeout, or a periodic pass the fleet queues has
// not completed within two periods and its timeout.
func (e *Engine) Probes() []Probe {
	var out []Probe
	for _, p := range e.engine.Probes() {
		out = append(out, Probe{Name: p.Name, Check: p.Check})
	}
	return out
}

// Route is one HTTP registration. Path is relative to the mount and uses
// net/http whole-segment wildcards; Handler reads Request.PathValue.
type Route struct {
	Method  string
	Path    string
	Handler http.Handler
}

// Routes returns the HTTP surface selection selects, with paths for the
// host's root router (adapters/http, adapters/gin and adapters/fiber mount
// them): the API under selection.Prefix and the admin console at its own path.
// It fails when the engine lacks what a selected group needs, before anything
// mounts. A standalone server's engine refuses: mount server.Routes.
func (e *Engine) Routes(selection Routes) ([]Route, error) {
	routes, err := e.engine.Routes(selection)
	if err != nil {
		return nil, err
	}
	out := make([]Route, len(routes))
	for i, r := range routes {
		out[i] = Route{Method: r.Method, Path: r.Path, Handler: r.Handler}
	}
	return out, nil
}

// PublicConfig is the document GET /v1/config serves on the mount of
// selection (the Routes the host mounts), read in process without a request.
// A host puts it in its own page response and passes it to billing-ui's
// BillingProvider as config, so the page makes no /v1/config request of its
// own. A standalone server's engine refuses: read the server's with
// Client.GetPublicConfig.
func (e *Engine) PublicConfig(selection Routes) (*billing.PublicConfig, error) {
	doc, err := e.engine.PublicConfig(selection)
	if err != nil {
		return nil, err
	}
	return &doc, nil
}

// CustomerConfig is the document GET /v1/me/config serves on the mount of
// selection to the customer r is signed in as: r is the host's own request,
// whose credential selection.Auth verifies as that mount does. A host puts it
// in its own page response and passes it to billing-ui's BillingProvider as
// customerConfig. A refusal is the mount's, a *billing.StatusError.
func (e *Engine) CustomerConfig(selection Routes, r *http.Request) (*billing.CustomerConfig, error) {
	if r == nil {
		return nil, invalidErr("the host's request is required")
	}
	status, body, err := e.engine.CustomerConfig(selection, r)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, statusErrorFromBody(status, body)
	}
	var doc billing.CustomerConfig
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

// RiverJobs is OpenRails' contribution to the host's River fleet (workers,
// periodic jobs, QueueBilling and QueuePSPImport), for riverhelpers.New in
// Config.Database.RiverSchema alongside the host's and AuthKit's; then pass the fleet to Start with
// WithRiverClient. Requesting it rules out OpenRails' own River.
func (e *Engine) RiverJobs() riverhelpers.Contribution {
	return e.engine.App.Runtime.RiverJobs()
}

// EnsurePSPOnboarding gives each live PSP of merchant its onboarding import
// now, restarting a failed one, as the engine's first load of the
// configuration does in the background. A tool calls it after New to read the
// onboarding import's latest state, or its restart, right away. A PSP another
// import is pulling or applying is passed over and named in an error
// matching billing.ErrPSPImportInProgress: call again once it finishes.
func (e *Engine) EnsurePSPOnboarding(ctx context.Context, merchant billing.MerchantID) error {
	return e.engine.App.Runtime.EnsurePSPOnboarding(ctx, merchant)
}

// QueueBilling and QueuePSPImport are the queues RiverJobs contributes, the
// latter for PSP import pulls and landings. Hosts may set their concurrency
// in the shared River config; each needs at least one worker.
const (
	QueueBilling   = riverjobs.QueueBilling
	QueuePSPImport = riverjobs.QueuePSPImport
)
