package app

import (
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	riverjobs "github.com/open-rails/openrails/internal/river"
)

// #689 worker health wiring: kinds are noted as workers register and periodic
// cadences as schedules are declared, so the health checker knows every kind
// and what "on time" means with zero per-worker code.

// workerHealthRegistrations lazily builds the runtime's registration set.
func (r *Runtime) workerHealthRegistrations() *riverjobs.WorkerRegistrations {
	r.workerHealthRegsOnce.Do(func() {
		r.workerHealthRegs = riverjobs.NewWorkerRegistrations()
	})
	return r.workerHealthRegs
}

// addTrackedWorker registers a worker and notes its kind for health seeding.
func addTrackedWorker[T river.JobArgs](r *Runtime, workers *river.Workers, worker river.Worker[T]) error {
	var args T
	r.workerHealthRegistrations().NoteKind(args.Kind())
	return river.AddWorkerSafely(workers, worker)
}

// healthPeriodic wraps river.NewPeriodicJob with a fixed interval, recording
// kind -> interval (shortest wins) for the staleness alert rule.
func (r *Runtime) healthPeriodic(interval time.Duration, ctor river.PeriodicJobConstructor, opts *river.PeriodicJobOpts) *river.PeriodicJob {
	if args, _ := ctor(); args != nil {
		r.workerHealthRegistrations().NotePeriod(args.Kind(), interval)
	}
	return river.NewPeriodicJob(river.PeriodicInterval(interval), ctor, opts)
}

// WorkerHealthMiddleware returns the per-job health bookkeeping middleware.
// The standalone client installs it automatically (InitRiver); embedded hosts
// running an external River client should add it to their client's
// river.Config.Middleware so their billing workers are covered too.
func (r *Runtime) WorkerHealthMiddleware() rivertype.Middleware {
	return riverjobs.NewWorkerHealthMiddleware(r.DB)
}
