package app

import (
	"context"
	"fmt"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	log "github.com/sirupsen/logrus"

	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/open-rails/openrails/internal/db/gen"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// Kinds are noted as workers register and cadences as schedules are declared,
// so the health checker knows every kind and its expected cadence with no
// per-worker code.
//
// The bookkeeping middleware is installed per worker, not on the client: River
// honours Worker.Middleware per job, so registering an OpenRails worker
// registers its bookkeeping and a host-owned client cannot omit it.

// workerHealthRegistrations lazily builds the runtime's registration set.
func (r *Runtime) workerHealthRegistrations() *riverjobs.WorkerRegistrations {
	r.workerHealthRegsOnce.Do(func() {
		r.workerHealthRegs = riverjobs.NewWorkerRegistrations()
	})
	return r.workerHealthRegs
}

// healthTrackedWorker wraps an OpenRails worker so its health bookkeeping
// travels with the worker itself rather than with whoever built the client.
type healthTrackedWorker[T river.JobArgs] struct {
	inner  river.Worker[T]
	health rivertype.WorkerMiddleware
}

func (w *healthTrackedWorker[T]) Middleware(job *rivertype.JobRow) []rivertype.WorkerMiddleware {
	return append([]rivertype.WorkerMiddleware{w.health}, w.inner.Middleware(job)...)
}

func (w *healthTrackedWorker[T]) NextRetry(job *river.Job[T]) time.Time {
	return w.inner.NextRetry(job)
}

// Timeout is the inner worker's: its kind's budget.
func (w *healthTrackedWorker[T]) Timeout(job *river.Job[T]) time.Duration {
	return w.inner.Timeout(job)
}

func (w *healthTrackedWorker[T]) Work(ctx context.Context, job *river.Job[T]) error {
	return w.inner.Work(ctx, job)
}

// addTrackedWorker registers a worker, notes its kind for health seeding, and
// attaches the health middleware to the worker itself.
func addTrackedWorker[T river.JobArgs](r *Runtime, workers *river.Workers, worker river.Worker[T]) error {
	var args T
	if got, want := worker.Timeout(&river.Job[T]{JobRow: &rivertype.JobRow{Kind: args.Kind()}}), riverjobs.Timeout(args.Kind()); got != want {
		return fmt.Errorf("%s: timeout %s is not its kind's budget %s", args.Kind(), got, want)
	}
	r.workerHealthRegistrations().NoteKind(args.Kind())
	return river.AddWorkerSafely[T](workers, &healthTrackedWorker[T]{
		inner:  worker,
		health: riverjobs.NewWorkerHealthMiddleware(r.DB),
	})
}

// periodicJob declares p with River. RunOnStart catches up an overdue pass;
// the shared helper's retained-history check skips a recent one after a
// restart. A leased pass is queued only by the fleet holding its lease.
func (r *Runtime) periodicJob(cfg *river.Config, p riverjobs.Periodic) *river.PeriodicJob {
	id := p.Args.Kind()
	r.workerHealthRegistrations().NotePeriod(id, p.Every)
	return riverhelpers.NewPeriodicJob(cfg, p.Every, func() (river.JobArgs, *river.InsertOpts) {
		if !p.PerFleet && !r.claimPeriodic(id, p.Every) {
			return nil, nil
		}
		return p.Args, nil
	}, &river.PeriodicJobOpts{ID: id, RunOnStart: true})
}

// claimPeriodic takes the periodic's lease for most of its period. Hosts
// sharing a database declare the same merchants and each schedule the same
// passes; the first to claim queues it and the others skip, so provider reads
// and operator alerts happen once. The lease expires, so a stopped fleet's
// passes are taken over by the next tick. A lease that cannot be read runs the
// pass: duplicate passes are safe, missed ones are not.
func (r *Runtime) claimPeriodic(id string, interval time.Duration) bool {
	if r.DB == nil {
		return true
	}
	holder := r.riverSchemaOrDefault()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n, err := r.DB.GenDirectory().ClaimPeriodicLease(ctx, gen.ClaimPeriodicLeaseParams{
		Kind: id, Holder: holder, TtlSeconds: (interval * 4 / 5).Seconds(),
	})
	if err != nil {
		log.WithError(err).WithField("periodic", id).Warn("periodic lease unreadable; running the pass")
		return true
	}
	if n == 0 {
		log.WithFields(log.Fields{"periodic": id, "fleet": holder}).Debug("periodic pass leased to another fleet; skipped")
		return false
	}
	return true
}
