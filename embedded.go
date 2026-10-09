package openrails

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/jackc/pgx/v5"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/engine"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// ErrRemoteClient refuses an operation only an embedded Client (New) offers.
var ErrRemoteClient = errors.New("openrails: not available on a remote client")

var errDerivedClose = errors.New("openrails: Close on a derived client; close the Client that New or NewRemote returned")

func init() {
	engine.Of = func(client any) *engine.Engine {
		if c, ok := client.(*Client); ok && c != nil {
			return c.engine
		}
		return nil
	}
}

// New runs the OpenRails engine in this process and returns the same Client
// NewRemote builds, over an in-process transport. Before anything else touches
// the database it creates or upgrades OpenRails' tables in
// Config.Database.Schema, River's in Config.Database.RiverSchema and this
// month's partitions; the pool's role owns them. That is idempotent, replicas
// booting together take turns on an advisory lock, and a schema a newer build
// already migrated is left as it is. It starts no workers: Start does, and
// jobs queued before it wait in Config.Database.RiverSchema. ctx bounds the
// wait for the database; nothing else does. With Config.Catalog, New applies
// it before returning and fails with the reason if it is refused; only provider
// references it cannot confirm within seconds finish in the background, and
// Ready fails until they do. Vault login, PSP posture checks and Redis recover
// in the background and fail only the features that need them (see Probes).
// opts are the options that make sense in process (WithTimeout, a default
// merchant). The credential and transport options belong to
// NewRemote and are refused: New authenticates as the host itself. For a
// customer's own credential over the same engine, use Client.With.
func New(ctx context.Context, cfg Config, deps Deps, opts ...ClientOption) (*Client, error) {
	requested := &Client{}
	for _, opt := range opts {
		if opt != nil {
			opt(requested)
		}
	}
	if requested.setupErr != nil {
		return nil, requested.setupErr
	}
	if requested.client != nil || requested.tokenFn != nil || requested.credentialFn != nil {
		return nil, fmt.Errorf("openrails: New runs in process and takes no WithHTTPClient, WithAPIKey, WithTokenProvider or WithCredentialProvider; use Client.With for a customer's own credential")
	}
	e, err := engine.New(ctx, cfg, deps)
	if err != nil {
		return nil, err
	}
	transport, capability := e.Transport()
	var options []ClientOption
	if id := e.ConfiguredMerchant(); !id.IsZero() {
		options = append(options, WithMerchantID(id))
	}
	options = append(options, opts...)
	options = append(options,
		WithHTTPClient(&http.Client{Transport: transport}),
		WithTokenProvider(func(context.Context) (string, error) { return capability, nil }))
	c, err := NewRemote(engine.InprocessBaseURL, options...)
	if err != nil {
		_ = e.Close(ctx)
		return nil, err
	}
	c.engine = e
	return c, nil
}

func (c *Client) embedded() (*engine.Engine, error) {
	if c == nil || c.engine == nil {
		return nil, ErrRemoteClient
	}
	return c.engine, nil
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
func (c *Client) Start(ctx context.Context, opts ...StartOption) error {
	e, err := c.embedded()
	if err != nil {
		return err
	}
	var o startOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	if o.fleetSet && o.fleet == nil {
		return fmt.Errorf("openrails: WithRiverClient requires a River client")
	}
	return e.Start(ctx, o.fleet)
}

// Close stops what Start started (OpenRails' own River, never the host's
// fleet) and closes the engine, leaving the host's pool, Redis and Vault
// clients open. On a remote client it releases idle connections. Only the Client that New or NewRemote returned closes: one
// derived from it (With) shares its engine and transport, and
// Close on it changes nothing and returns an error.
func (c *Client) Close(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if c.derived {
		return errDerivedClose
	}
	if c.engine == nil {
		c.client.CloseIdleConnections()
		return nil
	}
	return c.engine.Close(ctx)
}

// Ready reports whether the Client can serve. Embedded: Postgres, the merchant
// directory, Config.Catalog (applied) and River (the host's fleet bound to
// RiverJobs, or OpenRails' own running since Start); optional providers never
// fail it (see Probes). Remote: the server's /health/ready.
func (c *Client) Ready(ctx context.Context) error {
	if c.engine != nil {
		return c.engine.Ready(ctx)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health/ready", nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", billing.ErrUnreachable, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %s/health/ready answered %d", billing.ErrUnreachable, c.baseURL, resp.StatusCode)
	}
	return nil
}

// Probe checks one optional dependency.
type Probe struct {
	// Name is stable for dashboards: openrails_vault, openrails_psp_posture,
	// openrails_solana_signer_identity, openrails_job_progress.
	Name string
	// Check returns nil while the dependency is usable; it honors ctx.
	Check func(context.Context) error
}

// Probes lists the optional dependencies an embedded engine reconnects to in
// the background, for the host's dependency supervisor. openrails_vault is a
// live token lookup (only when OpenRails logs in to Vault itself).
// openrails_psp_posture fails while a PSP is unverified or disarmed.
// openrails_solana_signer_identity fails while a Transit signer key no longer
// matches its stored identity: the Solana rail refuses until an operator
// approves it (openrails solana-signer approve). openrails_job_progress fails
// while the River fleet is stalled. Remote clients have none.
func (c *Client) Probes() []Probe {
	if c == nil || c.engine == nil {
		return nil
	}
	var out []Probe
	for _, p := range c.engine.Probes() {
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
// mounts. With Config.ControlPlane it is the standalone surface, at the root.
func (c *Client) Routes(selection Routes) ([]Route, error) {
	e, err := c.embedded()
	if err != nil {
		return nil, err
	}
	routes, err := e.Routes(selection)
	if err != nil {
		return nil, err
	}
	out := make([]Route, len(routes))
	for i, r := range routes {
		out[i] = Route{Method: r.Method, Path: r.Path, Handler: r.Handler}
	}
	return out, nil
}

// CheckoutFrameAncestors is the Content-Security-Policy an embedded host sends
// with the payment page it serves: only the host itself and
// Config.Checkout.EmbedOrigins may frame it. The adapters'
// CheckoutFramePolicy sets it.
func (c *Client) CheckoutFrameAncestors() string {
	if c == nil || c.engine == nil {
		return "frame-ancestors 'self'"
	}
	return config.CheckoutFrameAncestors(c.engine.App.Config)
}

// RiverJobs is OpenRails' contribution to the host's River fleet (workers,
// periodic jobs and QueueBilling), for riverhelpers.New in
// Config.Database.RiverSchema alongside the host's and AuthKit's; then pass the fleet to Start with
// WithRiverClient. Requesting it rules out OpenRails' own River. It fails that
// composition on a remote client.
func (c *Client) RiverJobs() riverhelpers.Contribution {
	if c == nil || c.engine == nil {
		return riverhelpers.NewContribution("openrails", func(context.Context, *river.Config) error { return ErrRemoteClient }, nil, nil)
	}
	return c.engine.App.Runtime.RiverJobs()
}

// QueueBilling is the queue RiverJobs contributes. Hosts may set its
// concurrency in the shared River config; it needs at least one worker.
const QueueBilling = riverjobs.QueueBilling

// InvoiceSweepArgs is the invoice job OpenRails schedules on QueueBilling
// (daily period finalize, hourly collection, monthly-floor collection). A host
// whose fleet runs RiverJobs may insert one run itself, e.g.
// InvoiceSweepArgs{FinalizePreviousMonth: true} to finalize every payer's
// previous period now. Every run is idempotent.
type InvoiceSweepArgs struct {
	// MerchantID limits this run to one merchant; nil visits active merchants.
	MerchantID *billing.MerchantID `json:"merchant_id,omitempty"`
	// Collect runs the collection pass over open receivables.
	Collect bool `json:"collect,omitempty"`
	// UseMonthlyFloor collects down to the merchant's monthly floor instead of
	// its collection threshold.
	UseMonthlyFloor bool `json:"use_monthly_floor,omitempty"`
	// FinalizePreviousMonth finalizes each payer's previous billing period,
	// rating reported usage.
	FinalizePreviousMonth bool `json:"finalize_previous_month,omitempty"`
}

// Kind is the engine's invoice job kind.
func (InvoiceSweepArgs) Kind() string { return riverjobs.KindInvoice }

// InsertOpts targets QueueBilling.
func (InvoiceSweepArgs) InsertOpts() river.InsertOpts { return river.InsertOpts{Queue: QueueBilling} }

// DeclarePSP records a PSP identity, without credentials, for imported billing
// facts attributed to it. Call during setup, before serving or starting
// workers. It never arms the PSP for checkout. Embedded only.
func (c *Client) DeclarePSP(ctx context.Context, merchantID billing.MerchantID, declaration billing.PSPDeclaration) (*billing.PSP, error) {
	e, err := c.embedded()
	if err != nil {
		return nil, err
	}
	if e.App.Runtime == nil || e.App.Runtime.Merchants == nil {
		return nil, fmt.Errorf("openrails: runtime not initialized")
	}
	psp, err := e.App.Runtime.Merchants.DeclarePSP(ctx, merchantID, declaration)
	if err != nil {
		return nil, err
	}
	return &psp, nil
}

// OpenOperationAuthorizationTx is OpenOperationAuthorization inside tx, a
// transaction from the host's pool on the engine's database, so a provider
// obligation commits atomically with host rows. OpenRails binds the declared
// merchant and never commits or rolls tx back; after an error the host rolls
// back. Embedded only: a remote client returns ErrRemoteClient.
func (c *Client) OpenOperationAuthorizationTx(ctx context.Context, tx pgx.Tx, req billing.OpenOperationAuthorizationParams) (*billing.OperationAuthorization, error) {
	e, err := c.embedded()
	if err != nil {
		return nil, err
	}
	return e.OpenOperationAuthorizationTx(ctx, tx, req)
}

// GetOperationAuthorizationTx is GetOperationAuthorization inside tx (see
// OpenOperationAuthorizationTx). Embedded only.
func (c *Client) GetOperationAuthorizationTx(ctx context.Context, tx pgx.Tx, operationID string) (*billing.OperationAuthorization, error) {
	e, err := c.embedded()
	if err != nil {
		return nil, err
	}
	return e.GetOperationAuthorizationTx(ctx, tx, operationID)
}

// ExtendOperationAuthorizationTx is ExtendOperationAuthorization inside tx
// (see OpenOperationAuthorizationTx). Embedded only.
func (c *Client) ExtendOperationAuthorizationTx(ctx context.Context, tx pgx.Tx, req billing.ExtendOperationAuthorizationParams) (*billing.OperationAuthorizationExtension, error) {
	e, err := c.embedded()
	if err != nil {
		return nil, err
	}
	return e.ExtendOperationAuthorizationTx(ctx, tx, req)
}

// ReleaseOperationAuthorizationTx is ReleaseOperationAuthorization inside tx
// (see OpenOperationAuthorizationTx). Embedded only.
func (c *Client) ReleaseOperationAuthorizationTx(ctx context.Context, tx pgx.Tx, req billing.ReleaseOperationAuthorizationParams) (*billing.OperationAuthorization, error) {
	e, err := c.embedded()
	if err != nil {
		return nil, err
	}
	return e.ReleaseOperationAuthorizationTx(ctx, tx, req)
}

// RecordProviderBillingObservationTx is RecordProviderBillingObservation inside
// tx (see OpenOperationAuthorizationTx). Embedded only.
func (c *Client) RecordProviderBillingObservationTx(ctx context.Context, tx pgx.Tx, req billing.RecordProviderBillingObservationParams) (*billing.ProviderBillingQualification, error) {
	e, err := c.embedded()
	if err != nil {
		return nil, err
	}
	return e.RecordProviderBillingObservationTx(ctx, tx, req)
}

// GetProviderBillingQualificationTx is GetProviderBillingQualification inside
// tx (see OpenOperationAuthorizationTx). Embedded only.
func (c *Client) GetProviderBillingQualificationTx(ctx context.Context, tx pgx.Tx, operationID string) (*billing.ProviderBillingQualification, error) {
	e, err := c.embedded()
	if err != nil {
		return nil, err
	}
	return e.GetProviderBillingQualificationTx(ctx, tx, operationID)
}
