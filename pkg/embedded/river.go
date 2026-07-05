// Package embedded exposes OpenRails billing as an in-process library.
//
// # Schema contract (issues #165, #545)
//
// OpenRails owns a single configurable Postgres schema, set via config `db.schema`
// / env `DB_SCHEMA`, defaulting to `openrails`. It holds OpenRails' own DDL/DML —
// the portable billing data, and ONLY that.
//
// River job-queue tables (river_*) are runtime/infra state, NEVER portable billing
// data, so they NEVER live in the OpenRails billing schema. They follow these
// rules (#545):
//
//   - River tables ALWAYS live in `public` (config.RiverSchema) — River's own
//     documented default, alongside `public.migrations` and `pgcrypto`. This keeps
//     the OpenRails billing schema 100% portable for the embedded↔standalone data
//     move (#544).
//
//   - Standalone: OpenRails constructs its own River client in `public`.
//
//   - Embedded/library (this package): the HOST owns River. If the host runs River,
//     inject your unified client via SetRiverClient — OpenRails then enqueues
//     through it and NEVER constructs a River client (host + OpenRails share one
//     `public.river_*` set). If no client is injected, OpenRails constructs its own
//     River client in `public`.
//
// Migration safety: River does not auto-migrate its tables across schemas. Hosts
// or deployments still on an alternate River schema must drain and decommission
// the old `<schema>.river_*` objects when cutting over to `public`.
package embedded

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	riverjobs "github.com/open-rails/openrails/internal/river"
)

// ErrNotInitialized is returned when operations are attempted on an uninitialized Embedded instance.
var ErrNotInitialized = errors.New("embedded billing: not initialized")

// QueueBilling is the River queue name used by billing workers.
// Host applications should configure this queue when creating their River client.
//
// Example:
//
//	client, _ := river.NewClient(driver, river.Config{
//	    Workers: workers,
//	    Queues: map[string]river.QueueConfig{
//	        river.QueueDefault: {MaxWorkers: 10},
//	        embedded.QueueBilling: {MaxWorkers: 5},
//	    },
//	})
const QueueBilling = riverjobs.QueueBilling

// AddWorkersTo adds billing's River workers to the provided worker registry.
// Call this after creating your worker registry and adding your own workers.
//
// Example:
//
//	openrails, _ := embedded.New(opts)
//
//	workers := river.NewWorkers()
//	river.AddWorkerSafely(workers, &MyAppWorker{})
//	// Add billing workers
//	if err := openrails.AddWorkersTo(ctx, workers); err != nil {
//	    return err
//	}
//
//	client, _ := river.NewClient(driver, river.Config{Workers: workers})
func (e *Embedded) AddWorkersTo(ctx context.Context, workers *river.Workers) error {
	if e == nil || e.app == nil || e.app.Runtime == nil {
		return ErrNotInitialized
	}
	return e.app.Runtime.AddBillingWorkersTo(ctx, workers)
}

// GetPeriodicJobs returns billing's periodic jobs that should be added to your River client.
// Call this after creating your River client but before starting it.
//
// Example:
//
//	client, _ := river.NewClient(...)
//	periodicJobs, _ := openrails.GetPeriodicJobs(ctx)
//	for _, job := range periodicJobs {
//	    client.PeriodicJobs().Add(job)
//	}
//	client.Start(ctx)
func (e *Embedded) GetPeriodicJobs(ctx context.Context) ([]*river.PeriodicJob, error) {
	if e == nil || e.app == nil || e.app.Runtime == nil {
		return nil, ErrNotInitialized
	}
	return e.app.Runtime.GetBillingPeriodicJobs(ctx)
}

// WorkerMiddleware returns the #689 worker-health bookkeeping middleware. Add
// it to your River client's river.Config.Middleware so billing workers get
// per-kind health rows (and the health checker can alert on wedged workers):
//
//	client, _ := river.NewClient(driver, &river.Config{
//	    Workers:    workers,
//	    Middleware: []rivertype.Middleware{openrails.WorkerMiddleware()},
//	})
func (e *Embedded) WorkerMiddleware() rivertype.Middleware {
	if e == nil || e.app == nil || e.app.Runtime == nil {
		return nil
	}
	return e.app.Runtime.WorkerHealthMiddleware()
}

// SetRiverClient injects an external River client for billing to use for job enqueueing.
// Call this after creating your unified River client but before starting it.
//
// When an external client is provided:
//   - Billing will use it for enqueueing jobs (e.g., dunning, cleanup)
//   - Billing will NOT create its own River client
//   - RunWorkers() becomes a no-op (you're responsible for starting the client)
//   - The host client OWNS the River schema. River tables must live in `public`
//     (#545) so host and OpenRails share one `public.river_*` set; configure this
//     client accordingly. If you do NOT inject a client, OpenRails builds its own
//     in `public`.
//
// Example:
//
//	client, _ := river.NewClient(...)
//	openrails.SetRiverClient(client)
//	client.Start(ctx)
func (e *Embedded) SetRiverClient(client *river.Client[pgx.Tx]) {
	if e == nil || e.app == nil || e.app.Runtime == nil {
		return
	}
	e.app.Runtime.SetExternalRiverClient(client)
}

// HasExternalRiverClient returns true if an external River client was provided via SetRiverClient.
func (e *Embedded) HasExternalRiverClient() bool {
	if e == nil || e.app == nil || e.app.Runtime == nil {
		return false
	}
	return e.app.Runtime.HasExternalRiverClient()
}

// FoldIntoRiver folds billing's River workers and periodic jobs into a host's
// SHARED river.Workers/river.Client (#546), in the one order River's own API
// allows: Workers is fixed at river.NewClient, and periodic jobs must be
// registered before Client.Start. It replaces the AddWorkersTo/GetPeriodicJobs/
// SetRiverClient three-call dance (each host previously hand-wrote this as
// Service.RegisterRiverWorkers/RegisterRiverClient, ~40 near-identical lines).
//
// Call FoldIntoRiver once, with your own workers already added to workers,
// BEFORE building your river.Client:
//
//	workers := river.NewWorkers()
//	river.AddWorkerSafely(workers, &MyAppWorker{})
//	periodic, register, err := embedded.FoldIntoRiver(ctx, openrails, workers)
//	// err != nil: engine not initialized.
//
//	client, err := river.NewClient(driver, &river.Config{Workers: workers})
//	// periodic is billing's periodic jobs, exposed for logging/inspection —
//	// register (below) is what actually adds them to the client.
//	_ = periodic
//	if err := register(client); err != nil {
//	    // err != nil: client is nil.
//	}
//	client.Start(ctx)
//
// register (called AFTER NewClient, BEFORE Start) adds periodic to the client
// and calls SetRiverClient, injecting it so billing enqueues through it and
// never builds its own.
func FoldIntoRiver(ctx context.Context, e *Embedded, workers *river.Workers) (periodic []*river.PeriodicJob, register func(*river.Client[pgx.Tx]) error, err error) {
	if e == nil || e.app == nil || e.app.Runtime == nil {
		return nil, nil, ErrNotInitialized
	}
	if err := e.AddWorkersTo(ctx, workers); err != nil {
		return nil, nil, err
	}
	jobs, err := e.GetPeriodicJobs(ctx)
	if err != nil {
		return nil, nil, err
	}
	register = func(client *river.Client[pgx.Tx]) error {
		if client == nil {
			return fmt.Errorf("embedded billing: river client required")
		}
		for _, job := range jobs {
			_ = client.PeriodicJobs().Add(job)
		}
		e.SetRiverClient(client)
		return nil
	}
	return jobs, register, nil
}
