package app

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
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
	liveness   rivertype.WorkerMiddleware
	structural rivertype.WorkerMiddleware
}

// Middleware order matters. `health` is OUTERMOST so its bookkeeping records the
// error the queue actually acts on — i.e. the structural refusal, not the raw
// driver error underneath it (or#901) — and, inside it, the liveness reaper's
// NoProgressError rather than the bare context cancellation (xs-007 row 31).
func (w *healthTrackedWorker[T]) Middleware(job *rivertype.JobRow) []rivertype.WorkerMiddleware {
	inner := w.inner.Middleware(job)
	out := make([]rivertype.WorkerMiddleware, 0, len(inner)+3)
	out = append(out, w.health, w.liveness, w.structural)
	return append(out, inner...)
}

func (w *healthTrackedWorker[T]) NextRetry(job *river.Job[T]) time.Time {
	return w.inner.NextRetry(job)
}

// riverNoJobTimeout is River's spelling of "never cancel on elapsed time"
// (river.Config.JobTimeout docs: -1). It is the ONLY value an OpenRails worker
// may declare.
const riverNoJobTimeout = -1

// Timeout is -1 for every OpenRails worker, whatever the inner worker or the
// host's client says (xs-007 row 31). River resolves the job's clock as
// cmp.Or(worker.Timeout(), client.JobTimeout), so declaring it HERE — on the
// wrapper every OpenRails worker is registered through — is what makes a
// host-owned client's JobTimeout (River's default: 1 minute) unable to cancel
// billing work. Per-worker overrides would have to be remembered by every
// future worker; a client-level setting is the host's to forget. The wrapper
// is the one place that cannot be bypassed. A job ends on observed lack of
// progress (riverjobs.JobLivenessMiddleware), never on a clock.
func (w *healthTrackedWorker[T]) Timeout(*river.Job[T]) time.Duration {
	return riverNoJobTimeout
}

func (w *healthTrackedWorker[T]) Work(ctx context.Context, job *river.Job[T]) error {
	return w.inner.Work(ctx, job)
}

// addTrackedWorker registers a worker, notes its kind for health seeding, and
// attaches the health bookkeeping (#895) and liveness (xs-007 row 31)
// middlewares to the worker itself.
func addTrackedWorker[T river.JobArgs](r *Runtime, workers *river.Workers, worker river.Worker[T]) error {
	return addTrackedWorkerWithLiveness(r, workers, worker,
		riverjobs.NewJobLivenessMiddleware(r.riverTableAccess, r.workerHealthRegistrations()))
}

// addTrackedWorkerWithLiveness is addTrackedWorker with the liveness reaper
// supplied — tests hand in one with short beats.
func addTrackedWorkerWithLiveness[T river.JobArgs](r *Runtime, workers *river.Workers, worker river.Worker[T], liveness rivertype.WorkerMiddleware) error {
	var args T
	r.workerHealthRegistrations().NoteKind(args.Kind())
	return river.AddWorkerSafely[T](workers, &healthTrackedWorker[T]{
		inner:      worker,
		health:     riverjobs.NewWorkerHealthMiddleware(r.DB),
		liveness:   liveness,
		structural: riverjobs.NewStructuralFailureMiddleware(),
	})
}

// riverTableAccess resolves River's own pool and schema at beat time — on an
// embedded host both are bound after the workers were registered.
func (r *Runtime) riverTableAccess() (*pgxpool.Pool, string) {
	return r.riverStatsPool(), r.riverSchemaOrDefault()
}

// healthPeriodic wraps river.NewPeriodicJob with a fixed interval, recording
// kind -> interval (shortest wins) for the staleness alert rule.
func (r *Runtime) healthPeriodic(interval time.Duration, ctor river.PeriodicJobConstructor, opts *river.PeriodicJobOpts) *river.PeriodicJob {
	if args, _ := ctor(); args != nil {
		r.workerHealthRegistrations().NotePeriod(args.Kind(), interval)
	}
	return river.NewPeriodicJob(river.PeriodicInterval(interval), ctor, opts)
}
