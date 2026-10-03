package openrails

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/hosttools"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// ErrRemoteClient refuses an operation only an embedded Client (New) offers.
var ErrRemoteClient = errors.New("openrails: not available on a remote client")

func init() {
	engine.Of = func(client any) *engine.Engine {
		if c, ok := client.(*Client); ok && c != nil {
			return c.engine
		}
		return nil
	}
}

// Migrate creates or upgrades OpenRails' tables in cfg.Schema through pool.
// It is idempotent: run it on every boot, before New. The pool's role owns
// what it creates and is the role OpenRails runs as. With Config.River
// RiverManaged it also migrates River; a host-owned fleet migrates River itself.
func Migrate(ctx context.Context, pool *pgxpool.Pool, cfg Config) error {
	return engine.Migrate(ctx, pool, cfg)
}

// New runs the OpenRails engine in this process and returns the same Client
// NewRemote builds, over an in-process transport. ctx bounds the wait for the
// database; nothing else does. Vault login, PSP posture checks and Redis recover
// in the background and fail only the features that need them (see Probes).
// opts apply as they do to NewRemote (WithCurrency, WithTimeout, ...).
func New(ctx context.Context, cfg Config, deps Deps, opts ...ClientOption) (*Client, error) {
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

// Start starts OpenRails' workers; Close stops them. With RiverManaged that is
// OpenRails' own River fleet. With RiverHostOwned, pass RiverJobs to the
// host's fleet (riverhelpers.New) first; the host starts the fleet.
func (c *Client) Start(ctx context.Context) error {
	e, err := c.embedded()
	if err != nil {
		return err
	}
	return e.Start(ctx)
}

// Close stops the workers and closes the engine, leaving the host's pool,
// Redis and Vault clients open. On a remote client it releases idle
// connections.
func (c *Client) Close(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if c.engine == nil {
		c.client.CloseIdleConnections()
		return nil
	}
	return c.engine.Close(ctx)
}

// Ready reports whether the Client can serve. Embedded: Postgres, the merchant
// directory and River (bound when host-owned, running when managed); optional
// providers never fail it (see Probes). Remote: the server's /health/ready.
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

// Routes returns the HTTP surface Config.HTTP selects, for the host to mount
// (adapters/http, adapters/gin and adapters/fiber do). Mounted under /billing
// it serves /billing/v1/*. With Config.ControlPlane it is the standalone
// surface instead; see RoutesRequireRoot.
func (c *Client) Routes() ([]Route, error) {
	e, err := c.embedded()
	if err != nil {
		return nil, err
	}
	routes, err := e.Routes()
	if err != nil {
		return nil, err
	}
	out := make([]Route, len(routes))
	for i, r := range routes {
		out[i] = Route{Method: r.Method, Path: r.Path, Handler: r.Handler}
	}
	return out, nil
}

// RoutesRequireRoot reports whether Routes must be mounted at the router's
// root without a prefix: the control plane's issuer-anchored URLs do.
func (c *Client) RoutesRequireRoot() bool {
	return c != nil && c.engine != nil && c.engine.RoutesRequireRoot()
}

// RiverJobs is OpenRails' contribution to a host-owned River fleet (workers,
// periodic jobs and QueueBilling), for riverhelpers.New alongside the host's
// and AuthKit's. It fails that composition on a remote client.
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
// that owns the fleet may insert one run itself, e.g.
// InvoiceSweepArgs{FinalizePreviousMonth: true} to finalize every payer's
// previous period now. Every run is idempotent.
type InvoiceSweepArgs struct {
	// Collect runs the collection pass over open receivables.
	Collect bool `json:"collect,omitempty"`
	// CollectionThresholdAmount overrides the merchant's collection trigger
	// (minor units); 0 keeps the merchant setting.
	CollectionThresholdAmount int64 `json:"collection_threshold_amount,omitempty"`
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
// workers. It never arms the PSP for checkout.
func (c *Client) DeclarePSP(ctx context.Context, merchantID billing.MerchantID, declaration billing.PSPDeclaration) (uuid.UUID, error) {
	e, err := c.embedded()
	if err != nil {
		return uuid.Nil, err
	}
	return hosttools.DeclarePSP(ctx, e.App, merchantID, declaration)
}

// The Tx operations run the same commands as their Client counterparts inside
// tx, a transaction from the host's pool on the engine's database, for provider
// obligations that must commit atomically with host rows. OpenRails binds the
// declared merchant and never commits or rolls tx back; after an error the
// host rolls back. Embedded only.

func (c *Client) OpenOperationAuthorizationTx(ctx context.Context, tx pgx.Tx, req billing.OperationAuthorizationRequest) (*billing.OperationAuthorization, error) {
	e, err := c.embedded()
	if err != nil {
		return nil, err
	}
	return e.OpenOperationAuthorizationTx(ctx, tx, req)
}

func (c *Client) GetOperationAuthorizationTx(ctx context.Context, tx pgx.Tx, operationID string) (*billing.OperationAuthorization, error) {
	e, err := c.embedded()
	if err != nil {
		return nil, err
	}
	return e.GetOperationAuthorizationTx(ctx, tx, operationID)
}

func (c *Client) ReleaseOperationAuthorizationTx(ctx context.Context, tx pgx.Tx, req billing.ReleaseOperationAuthorizationRequest) (*billing.OperationAuthorization, error) {
	e, err := c.embedded()
	if err != nil {
		return nil, err
	}
	return e.ReleaseOperationAuthorizationTx(ctx, tx, req)
}

func (c *Client) RecordProviderBillingObservationTx(ctx context.Context, tx pgx.Tx, req billing.ProviderBillingObservationRequest) (*billing.ProviderBillingQualification, error) {
	e, err := c.embedded()
	if err != nil {
		return nil, err
	}
	return e.RecordProviderBillingObservationTx(ctx, tx, req)
}

func (c *Client) GetProviderBillingQualificationTx(ctx context.Context, tx pgx.Tx, operationID string) (*billing.ProviderBillingQualification, error) {
	e, err := c.embedded()
	if err != nil {
		return nil, err
	}
	return e.GetProviderBillingQualificationTx(ctx, tx, operationID)
}
