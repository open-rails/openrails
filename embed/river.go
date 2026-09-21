// Package embed exposes OpenRails billing as an in-process library.
//
// # River is a REQUIRED dependency (#895)
//
// OpenRails' River periodic fleet is not auxiliary — it is where the money
// moves: subscription_converge, credit_expiry, invoice,
// credit_reconcile, stripe_webhook_reconcile, provider_intent_execute,
// ledger_integrity, the Solana cranks. With River absent every read API keeps
// answering correctly and the money silently stops: subscriptions never renew,
// credits never expire, invoices are never cut, webhooks are never reconciled.
// There is no degraded mode worth having, so River is not optional.
//
// Options.River defaults to OpenRails ownership. RiverFromHost() gives the
// host ownership; BindRiver receives a fully composed config and constructs one
// unstarted client after components attach. Otherwise OpenRails constructs its client and the caller runs
// RunWorkers (or sets Options.RunWorkers).
//
// # Schema contract (issues #165, #545)
//
// OpenRails owns a single configurable Postgres schema, set via config `db.schema`
// / env `DB_SCHEMA`, defaulting to `billing`. It holds OpenRails' own DDL/DML —
// the portable billing data, and ONLY that.
//
// River job-queue tables (river_*) are runtime/infra state, NEVER portable billing
// data, so they NEVER live in the OpenRails billing schema — that is what keeps
// the billing schema 100% portable for the embedded<->standalone data move (#544).
//
// WHERE the river_* set lives is the client owner's call. When OpenRails
// constructs its own client (standalone, or embedded without an injected
// client) it defaults to `public` (config.RiverSchema), alongside the migration
// ledger and shared extensions. RiverManagedByOpenRails(schema) overrides that
// namespace. A host-injected client owns its schema: the engine adopts client.Schema() for everything it
// does with River (progress detection included), refusing only a schema that
// collides with the billing schema. Two applications embedding OpenRails in
// one database each keep their own river_* set this way; a shared set would
// share river_leader, and River's elected leader schedules only its own
// app's periodic jobs.
//
// Migration safety: River does not auto-migrate its tables across schemas. A
// host that changes its client's schema must drain and decommission the old
// `<schema>.river_*` objects itself.
//
// # Liveness
//
// Construction refusing only covers "not wired at all". "Wired but not
// progressing" is a live property, so OpenRails runs its own out-of-River
// detector (see CheckJobProgress) that reads River's `river_job` watermarks from
// a plain goroutine — it keeps reporting while River is wedged or never started.
package embed

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/riverkit"
	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/config"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// ErrNotInitialized is returned when operations are attempted on an uninitialized Embedded instance.
var ErrNotInitialized = errors.New("embedded billing: not initialized")

// QueueBilling is the billing queue already present in BindRiver's composed
// config. Hosts may adjust its concurrency; at least one worker is required.
const QueueBilling = riverjobs.QueueBilling

// InvoiceSweepArgs is the invoice job OpenRails schedules on the billing queue
// (daily period finalize, hourly collection, 30-day monthly-floor collection),
// as public River job args so a host that owns the fleet (RiverFromHost) can
// insert one run itself: `jobs.Insert(ctx, embed.InvoiceSweepArgs{
// FinalizePreviousMonth: true}, nil)` finalizes every payer's previous period
// now, worked by the same InvoiceWorker on the same registry. Every run is
// idempotent, so an extra run never double-bills. This is the only engine
// job a host inserts; the other kinds are engine-internal schedules.
type InvoiceSweepArgs struct {
	// Collect runs the collection pass over open receivables.
	Collect bool `json:"collect,omitempty"`
	// CollectionThresholdAmount overrides the merchant's collection trigger
	// (minor units); 0 keeps the merchant setting.
	CollectionThresholdAmount int64 `json:"collection_threshold_amount,omitempty"`
	// UseMonthlyFloor collects down to the merchant's monthly floor instead of
	// its collection threshold.
	UseMonthlyFloor bool `json:"use_monthly_floor,omitempty"`
	// FinalizePreviousMonth finalizes each payer's previous billing period
	// under the merchant's billing_period_boundary, rating reported usage.
	FinalizePreviousMonth bool `json:"finalize_previous_month,omitempty"`
}

// Kind is the engine's invoice job kind, so the host's insert is worked by
// OpenRails' InvoiceWorker.
func (InvoiceSweepArgs) Kind() string { return riverjobs.KindInvoice }

// InsertOpts targets the billing queue by default; a host may still pass
// explicit river.InsertOpts to Insert.
func (InvoiceSweepArgs) InsertOpts() river.InsertOpts { return river.InsertOpts{Queue: QueueBilling} }

// RiverOwnership declares who owns the River fleet. Its zero value lets
// OpenRails manage River in public. Use the same value for New and ApplyMigrations.
type RiverOwnership struct {
	host   bool
	schema string
	err    error
}

// RiverFromHost gives the host ownership of River's migrations and client
// lifecycle. Call Runtime.BindRiver after attaching every component.
func RiverFromHost() RiverOwnership { return RiverOwnership{host: true} }

// RiverManagedByOpenRails selects OpenRails ownership, optionally in a separate
// schema (default public). The caller runs RunWorkers to start the fleet.
func RiverManagedByOpenRails(schema ...string) RiverOwnership {
	o := RiverOwnership{}
	if len(schema) > 1 {
		o.err = fmt.Errorf("embedded billing: at most one River schema is allowed")
	} else if len(schema) == 1 {
		o.schema = schema[0]
	}
	return o
}

func (o RiverOwnership) managedSchema(billingSchema string) (string, error) {
	if o.err != nil {
		return "", o.err
	}
	if o.host {
		return "", nil
	}
	schema := strings.TrimSpace(o.schema)
	if schema == "" {
		schema = config.RiverSchema
	}
	schema, err := validateMigrationSchema(schema)
	if err != nil {
		return "", fmt.Errorf("embedded billing: River schema: %w", err)
	}
	if schema == billingSchema {
		return "", fmt.Errorf("embedded billing: River schema %q must differ from the billing schema", schema)
	}
	return schema, nil
}

// RiverJobs contributes billing and any already attached control-plane jobs.
// Attach components first, then pass this contribution to riverkit.New alongside
// other libraries. The host owns the returned client's Start/Stop lifecycle.
func (r *Runtime) RiverJobs() riverkit.Contribution {
	if r == nil || r.app == nil || r.app.Runtime == nil {
		return riverkit.NewContribution("openrails", func(context.Context, *river.Config) error { return ErrNotInitialized }, nil, nil)
	}
	return r.app.Runtime.RiverJobs()
}

// HasExternalRiverClient reports whether BindRiver has successfully bound the
// host-owned client. Ownership declaration alone returns false.
func (r *Runtime) HasExternalRiverClient() bool {
	if r == nil || r.app == nil || r.app.Runtime == nil {
		return false
	}
	return r.app.Runtime.HasExternalRiverClient()
}

// JobProgress is one evaluation of the periodic fleet.
type JobProgress = riverjobs.ProgressReport

// CheckJobProgress answers "are OpenRails' cron jobs actually progressing?"
// RIGHT NOW, from a path that does not require a job to run (#895): it reads
// River's own `river_job` watermarks plus the per-kind health rows. Safe to call
// from a host health endpoint.
//
// A host is not REQUIRED to call it — OpenRails runs the same check on its own
// goroutine and raises durable repair alerts — but a host that wants the verdict
// on its own /healthz can have it:
//
//	report, err := openrails.CheckJobProgress(ctx)
//	if err == nil {
//	    err = report.Err() // non-nil when the fleet is stalled
//	}
func (r *Runtime) CheckJobProgress(ctx context.Context) (JobProgress, error) {
	if r == nil || r.app == nil || r.app.Runtime == nil {
		return JobProgress{}, ErrNotInitialized
	}
	return r.app.Runtime.RiverProgress(ctx)
}

// resolveHostRiverSchema decides the schema the engine uses for everything it
// does with River, from the host client's own schema. Empty means River's
// default. The billing schema is refused: river_* is runtime state, and
// placing it inside the portable billing schema breaks the
// embedded<->standalone data move the schema exists for.
func resolveHostRiverSchema(clientSchema, billingSchema string) (string, error) {
	schema := strings.TrimSpace(clientSchema)
	if schema == "" {
		schema = config.RiverSchema
	}
	if schema == billingSchema {
		return "", fmt.Errorf("embedded billing: host River client uses the billing schema %q for river_* tables — River state is not portable billing data and must live elsewhere (default %q)", billingSchema, config.RiverSchema)
	}
	return schema, nil
}
