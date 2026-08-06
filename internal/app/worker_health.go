package app

import (
	"context"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	riverjobs "github.com/open-rails/openrails/internal/river"
)

// #689 worker health wiring: kinds are noted as workers register and periodic
// cadences as schedules are declared, so the health checker knows every kind
// and what "on time" means with zero per-worker code.
//
// #895: the bookkeeping middleware is installed PER WORKER, at registration,
// not on the client. It used to be a client-level river.Config.Middleware
// entry, which meant an embedded host could adopt the fleet via AddWorkersTo
// and simply omit it — measured on two hosts, where every periodic kind then
// reported never_succeeded forever (19/25 on cozy-art, 20/24 on tensorhub:
// 100% false alarms, which trains operators to ignore the real signal). River
// honours Worker.Middleware(job) per work unit (internal/jobexecutor), so
// attaching it here makes the omission unrepresentable: registering an
// OpenRails worker registers its bookkeeping, with no host cooperation.

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
	inner      river.Worker[T]
	health     rivertype.WorkerMiddleware
	structural rivertype.WorkerMiddleware
}

// Middleware order matters. `health` is OUTERMOST so its bookkeeping records the
// error the queue actually acts on — i.e. the structural refusal, not the raw
// driver error underneath it (or#901).
func (w *healthTrackedWorker[T]) Middleware(job *rivertype.JobRow) []rivertype.WorkerMiddleware {
	inner := w.inner.Middleware(job)
	out := make([]rivertype.WorkerMiddleware, 0, len(inner)+2)
	out = append(out, w.health, w.structural)
	return append(out, inner...)
}

func (w *healthTrackedWorker[T]) NextRetry(job *river.Job[T]) time.Time {
	return w.inner.NextRetry(job)
}

func (w *healthTrackedWorker[T]) Timeout(job *river.Job[T]) time.Duration {
	return w.inner.Timeout(job)
}

func (w *healthTrackedWorker[T]) Work(ctx context.Context, job *river.Job[T]) error {
	return w.inner.Work(ctx, job)
}

// addTrackedWorker registers a worker, notes its kind for health seeding, and
// attaches the health bookkeeping middleware to the worker itself (#895).
func addTrackedWorker[T river.JobArgs](r *Runtime, workers *river.Workers, worker river.Worker[T]) error {
	var args T
	r.workerHealthRegistrations().NoteKind(args.Kind())
	return river.AddWorkerSafely[T](workers, &healthTrackedWorker[T]{
		inner:      worker,
		health:     riverjobs.NewWorkerHealthMiddleware(r.DB),
		structural: riverjobs.NewStructuralFailureMiddleware(),
	})
}

// healthPeriodic wraps river.NewPeriodicJob with a fixed interval, recording
// kind -> interval (shortest wins) for the staleness alert rule.
func (r *Runtime) healthPeriodic(interval time.Duration, ctor river.PeriodicJobConstructor, opts *river.PeriodicJobOpts) *river.PeriodicJob {
	if args, _ := ctor(); args != nil {
		r.workerHealthRegistrations().NotePeriod(args.Kind(), interval)
	}
	return river.NewPeriodicJob(river.PeriodicInterval(interval), ctor, opts)
}
