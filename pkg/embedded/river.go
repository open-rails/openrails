// Package embedded exposes OpenRails billing as an in-process library.
//
// # Schema contract (issue #165)
//
// OpenRails owns a single configurable Postgres schema, set via config `db.schema`
// / env `DB_SCHEMA`, defaulting to `billing`. It is used for OpenRails' own DDL/DML.
//
// River job-queue tables (river_*) follow these rules:
//
//   - Standalone: OpenRails constructs its own River client, and the River schema
//     equals the OpenRails schema (db.schema). It is NOT separately configurable.
//
//   - Embedded/library (this package): the HOST owns River. Inject your unified
//     River client via SetRiverClient — OpenRails then enqueues through it and
//     NEVER constructs a River client or assumes/overrides a River schema. Your
//     injected client owns its schema entirely, so OpenRails' River jobs land
//     wherever your client is configured (e.g. the host's primary schema), which
//     may differ from OpenRails' db.schema. This lets a host keep billing tables
//     under a billing schema while sharing River tables in its own schema.
//
// Migration safety: switching db.schema does not move existing tables — it creates
// a second set under the new schema. OpenRails does not auto-migrate River tables
// across schemas. If you change schemas, decommission the old objects yourself.
package embedded

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

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

// SetRiverClient injects an external River client for billing to use for job enqueueing.
// Call this after creating your unified River client but before starting it.
//
// When an external client is provided:
//   - Billing will use it for enqueueing jobs (e.g., dunning, cleanup)
//   - Billing will NOT create its own River client
//   - RunWorkers() becomes a no-op (you're responsible for starting the client)
//   - The host client OWNS the River schema. OpenRails neither assumes nor
//     overrides it (issue #165), so River tables live wherever you configured
//     this client — independent of OpenRails' db.schema.
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
