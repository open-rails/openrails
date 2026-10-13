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
// jobs queued before it wait in Config.Database.RiverSchema. ctx bounds the
// wait for the database; nothing else does. It provisions each merchant
// Config.Merchants declares and never changes a catalog: a catalog document
// is an operator's ApplyCatalog. Vault login, PSP posture checks and Redis
// recover in the background and fail only the features that need them (see
// Probes).
func New(ctx context.Context, cfg Config, deps Deps) (*Engine, error) {
	if _, err := readPoliciesFor(inProcess, cfg.ReadCache); err != nil {
		return nil, err
	}
	inner, err := engine.New(ctx, cfg, deps)
	if err != nil {
		return nil, err
	}
	e := &Engine{engine: inner, client: &http.Client{Transport: inner.Transport()}}
	if e.reads, err = newReads(inProcess, cfg.ReadCache, 0, e.handle); err != nil {
		return nil, errors.Join(err, inner.Close(ctx))
	}
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
	// openrails_solana_signer_identity, openrails_job_progress.
	Name string
	// Check returns nil while the dependency is usable; it honors ctx.
	Check func(context.Context) error
}

// Probes lists the optional dependencies the engine reconnects to in the
// background, for the host's dependency supervisor. openrails_vault is a
// live token lookup (only when OpenRails logs in to Vault itself).
// openrails_psp_posture fails while a PSP is unverified or disarmed.
// openrails_solana_signer_identity fails while a Transit signer key no longer
// matches its stored identity: the Solana rail refuses until an operator
// approves it (openrails solana-signer approve). openrails_job_progress fails
// while the River fleet is stalled.
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
// periodic jobs and QueueBilling), for riverhelpers.New in
// Config.Database.RiverSchema alongside the host's and AuthKit's; then pass the fleet to Start with
// WithRiverClient. Requesting it rules out OpenRails' own River.
func (e *Engine) RiverJobs() riverhelpers.Contribution {
	return e.engine.App.Runtime.RiverJobs()
}

// QueueBilling is the queue RiverJobs contributes. Hosts may set its
// concurrency in the shared River config; it needs at least one worker.
const QueueBilling = riverjobs.QueueBilling

// UsageSweepArgs is the usage-order job OpenRails schedules on QueueBilling
// (daily billing of the previous period, hourly collection, monthly-floor
// collection). A host whose fleet runs RiverJobs may insert one run itself,
// e.g. UsageSweepArgs{FinalizePreviousMonth: true} to bill every customer's
// previous period now. Every run is idempotent.
type UsageSweepArgs struct {
	// MerchantID limits this run to one merchant; nil visits active merchants.
	MerchantID *billing.MerchantID `json:"merchant_id,omitempty"`
	// Collect runs the collection pass over unpaid usage orders.
	Collect bool `json:"collect,omitempty"`
	// UseMonthlyFloor collects down to the merchant's monthly floor instead of
	// its collection threshold.
	UseMonthlyFloor bool `json:"use_monthly_floor,omitempty"`
	// FinalizePreviousMonth bills each customer's previous billing period,
	// rating reported usage.
	FinalizePreviousMonth bool `json:"finalize_previous_month,omitempty"`
}

// Kind is the engine's usage-order job kind.
func (UsageSweepArgs) Kind() string { return riverjobs.KindUsageOrders }

// InsertOpts targets QueueBilling.
func (UsageSweepArgs) InsertOpts() river.InsertOpts { return river.InsertOpts{Queue: QueueBilling} }
