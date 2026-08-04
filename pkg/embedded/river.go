// Package embedded exposes OpenRails billing as an in-process library.
//
// # River is a REQUIRED dependency (#895)
//
// OpenRails' River periodic fleet is not auxiliary — it is where the money
// moves: subscription_converge, credit_expiry, auto_topup, invoice,
// credit_reconcile, stripe_webhook_reconcile, provider_intent_execute,
// ledger_integrity, the Solana cranks. With River absent every read API keeps
// answering correctly and the money silently stops: subscriptions never renew,
// credits never expire, invoices are never cut, webhooks are never reconciled.
// There is no degraded mode worth having, so River is not optional.
//
// Options.River is therefore MANDATORY and has exactly two values — there is no
// third state that silently means "nobody owns the fleet":
//
//   - RiverFromHost(bind): the HOST owns River. bind is called during New with
//     OpenRails' workers already registered; it must return a live, unstarted
//     *river.Client. Returning nil, or an error, fails construction. This is the
//     embedded posture, and the only way to declare host ownership: a host can
//     no longer claim the fleet and then never deliver it.
//   - RiverManagedByOpenRails(): OpenRails constructs and runs its own client
//     (the standalone posture). The caller must run RunWorkers.
//
// # Schema contract (issues #165, #545)
//
// OpenRails owns a single configurable Postgres schema, set via config `db.schema`
// / env `DB_SCHEMA`, defaulting to `openrails`. It holds OpenRails' own DDL/DML —
// the portable billing data, and ONLY that.
//
// River job-queue tables (river_*) are runtime/infra state, NEVER portable billing
// data, so they NEVER live in the OpenRails billing schema. River tables ALWAYS
// live in `public` (config.RiverSchema) — River's own documented default,
// alongside `public.migrations` and `pgcrypto`. This keeps the OpenRails billing
// schema 100% portable for the embedded<->standalone data move (#544). A
// host-supplied client whose Schema() is not `public` is rejected by New.
//
// Migration safety: River does not auto-migrate its tables across schemas. Hosts
// or deployments still on an alternate River schema must drain and decommission
// the old `<schema>.river_*` objects when cutting over to `public`.
//
// # Liveness
//
// Construction refusing only covers "not wired at all". "Wired but not
// progressing" is a live property, so OpenRails runs its own out-of-River
// detector (see CheckJobProgress) that reads River's `river_job` watermarks from
// a plain goroutine — it keeps reporting while River is wedged or never started.
package embedded

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/config"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// ErrNotInitialized is returned when operations are attempted on an uninitialized Embedded instance.
var ErrNotInitialized = errors.New("embedded billing: not initialized")

// ErrRiverRequired is returned by New when Options.River is unset (#895).
var ErrRiverRequired = errors.New("embedded billing: Options.River is required (#895) — declare RiverFromHost(bind) to inject your River client, or RiverManagedByOpenRails() to let OpenRails construct and run its own; OpenRails cannot function without River, so there is no third state")

// QueueBilling is the River queue name used by billing workers. A host-owned
// client MUST configure it, or billing jobs are inserted and never worked.
//
// Example:
//
//	river.NewClient(driver, &river.Config{
//	    Queues: map[string]river.QueueConfig{
//	        river.QueueDefault:    {MaxWorkers: 10},
//	        embedded.QueueBilling: {MaxWorkers: 5},
//	    },
//	})
const QueueBilling = riverjobs.QueueBilling

// RiverFleet is handed to a RiverBinder during New. Workers already holds every
// OpenRails billing worker, each with its health bookkeeping attached (#895), so
// there is nothing for the host to remember to install. Add your own workers to
// it, then build your client from it.
type RiverFleet struct {
	// Workers is the SHARED registry. River fixes Workers at NewClient time, so
	// this is the one moment host and billing workers can be merged.
	Workers *river.Workers
	// QueueBilling is the queue OpenRails' jobs are inserted on; configure it on
	// the client you return.
	QueueBilling string
	// Schema is the schema OpenRails expects River's tables in; your client's
	// river.Config.Schema must match (empty means River's default, `public`).
	Schema string
}

// RiverBinder builds the host's River client from the shared fleet. It must
// return a constructed but NOT-yet-started client: OpenRails registers its own
// periodic jobs on it before the host starts it, so the host cannot omit them.
type RiverBinder func(ctx context.Context, fleet *RiverFleet) (*river.Client[pgx.Tx], error)

// RiverOwnership declares who owns the River fleet. Build it with
// RiverFromHost or RiverManagedByOpenRails; the zero value is the rejected
// "nobody" state.
type RiverOwnership struct {
	managed bool
	bind    RiverBinder
}

func (o RiverOwnership) declared() bool { return o.managed || o.bind != nil }

// RiverFromHost declares that the HOST owns River and hands OpenRails the
// client. bind is invoked during New; if it returns an error or a nil client,
// construction fails.
func RiverFromHost(bind RiverBinder) RiverOwnership {
	return RiverOwnership{bind: bind}
}

// RiverManagedByOpenRails declares that OpenRails constructs and runs its own
// River client — the standalone posture. The caller MUST run RunWorkers, or the
// fleet never starts (and the progress monitor will say so).
func RiverManagedByOpenRails() RiverOwnership {
	return RiverOwnership{managed: true}
}

// bindRiver runs the host's binder and folds the result into the engine. Called
// once, from New, after the application graph exists.
func (e *Embedded) bindRiver(ctx context.Context, own RiverOwnership) error {
	rt := e.app.Runtime
	if own.managed {
		// Populate the health registrations (kind -> declared cadence) NOW, even
		// though standalone builds its client later in RunWorkers: the progress
		// monitor needs to know what "expected" means before the fleet starts, or
		// "declared managed and then never ran RunWorkers" is invisible too.
		if _, err := rt.GetBillingPeriodicJobs(ctx); err != nil {
			return fmt.Errorf("build billing periodic jobs: %w", err)
		}
		return nil
	}
	workers := river.NewWorkers()
	if err := rt.AddBillingWorkersTo(ctx, workers); err != nil {
		return fmt.Errorf("register billing workers: %w", err)
	}
	periodic, err := rt.GetBillingPeriodicJobs(ctx)
	if err != nil {
		return fmt.Errorf("build billing periodic jobs: %w", err)
	}
	client, err := own.bind(ctx, &RiverFleet{
		Workers:      workers,
		QueueBilling: QueueBilling,
		Schema:       config.RiverSchema,
	})
	if err != nil {
		return fmt.Errorf("embedded billing: River binder failed (#895): %w", err)
	}
	if client == nil {
		return fmt.Errorf("embedded billing: River binder returned a nil client (#895) — OpenRails cannot run its periodic fleet, and every money-moving job would silently never run")
	}
	if schema := client.Schema(); schema != "" && schema != config.RiverSchema {
		return fmt.Errorf("embedded billing: host River client uses schema %q, OpenRails requires %q (#545) — host and billing must share one river_* set", schema, config.RiverSchema)
	}
	// OpenRails registers its OWN periodic jobs on the host's client. This is
	// deliberately not the host's job: a host that registered workers but not
	// schedules would have a fleet that can work but is never asked to.
	for _, job := range periodic {
		client.PeriodicJobs().Add(job)
	}
	rt.SetExternalRiverClient(client)
	return nil
}

// HasExternalRiverClient reports whether the host owns the River client
// (RiverFromHost) rather than OpenRails (RiverManagedByOpenRails).
func (e *Embedded) HasExternalRiverClient() bool {
	if e == nil || e.app == nil || e.app.Runtime == nil {
		return false
	}
	return e.app.Runtime.HasExternalRiverClient()
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
func (e *Embedded) CheckJobProgress(ctx context.Context) (JobProgress, error) {
	if e == nil || e.app == nil || e.app.Runtime == nil {
		return JobProgress{}, ErrNotInitialized
	}
	return e.app.Runtime.RiverProgress(ctx)
}
