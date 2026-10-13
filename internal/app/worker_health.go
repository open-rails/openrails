package app

import (
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	log "github.com/sirupsen/logrus"

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
	inner      river.Worker[T]
	health     rivertype.WorkerMiddleware
	liveness   rivertype.WorkerMiddleware
	structural rivertype.WorkerMiddleware
}

// Middleware puts health outermost, so it records the error the queue acts
// on: the structural refusal rather than the raw driver error, and the liveness
// reaper's NoProgressError rather than a bare context cancellation.
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
// host's client says. River resolves cmp.Or(worker.Timeout(),
// client.JobTimeout), so declaring it on the wrapper every worker registers
// through stops a host client's JobTimeout (default 1 minute) from cancelling
// billing work. A job ends on observed lack of progress
// (riverjobs.JobLivenessMiddleware), never on a clock.
func (w *healthTrackedWorker[T]) Timeout(*river.Job[T]) time.Duration {
	return riverNoJobTimeout
}

func (w *healthTrackedWorker[T]) Work(ctx context.Context, job *river.Job[T]) error {
	return w.inner.Work(ctx, job)
}

// addTrackedWorker registers a worker, notes its kind for health seeding, and
// attaches the health, liveness and structural-failure middlewares to the
// worker itself.
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
// kind -> interval (shortest wins) for the staleness alert rule. A periodic's
// River ID is its job kind unless opts names one, so a River client can turn
// a single periodic off (PeriodicJobs().RemoveByID). The pass is queued only
// by the fleet holding the kind's lease (see claimPeriodic).
func (r *Runtime) healthPeriodic(interval time.Duration, ctor river.PeriodicJobConstructor, opts *river.PeriodicJobOpts) *river.PeriodicJob {
	return r.periodic(true, interval, ctor, opts)
}

// fleetPeriodic is a periodic over this fleet's own state (its River jobs, its
// Redis): every fleet runs it.
func (r *Runtime) fleetPeriodic(interval time.Duration, ctor river.PeriodicJobConstructor, opts *river.PeriodicJobOpts) *river.PeriodicJob {
	return r.periodic(false, interval, ctor, opts)
}

func (r *Runtime) periodic(leased bool, interval time.Duration, ctor river.PeriodicJobConstructor, opts *river.PeriodicJobOpts) *river.PeriodicJob {
	own := river.PeriodicJobOpts{}
	if opts != nil {
		own = *opts
	}
	if args, _ := ctor(); args != nil {
		r.workerHealthRegistrations().NotePeriod(args.Kind(), interval)
		if own.ID == "" {
			own.ID = args.Kind()
		}
	}
	r.periodicIDs = append(r.periodicIDs, own.ID)
	if leased {
		inner, id := ctor, own.ID
		ctor = func() (river.JobArgs, *river.InsertOpts) {
			if !r.claimPeriodic(id, interval) {
				return nil, nil
			}
			return inner()
		}
	}
	return river.NewPeriodicJob(periodBoundaries(interval), ctor, &own)
}

// claimPeriodic takes the periodic's lease for most of its period. Several
// hosts embedding one merchant on one database each schedule the same passes;
// the first to claim queues it and the others skip, so provider reads and
// operator alerts happen once. The lease expires, so a stopped fleet's passes
// are taken over by the next tick. A lease that cannot be read runs the pass:
// duplicate passes are safe, missed ones are not.
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

// PeriodicJobIDs are the River IDs of the periodic jobs this runtime gave its
// River client.
func (r *Runtime) PeriodicJobIDs() []string { return slices.Clone(r.periodicIDs) }

// periodBoundaries fires at each multiple of the period on the clock, where
// a ByPeriod uniqueness bucket starts. Only the River leader schedules, and a
// new leader starts its schedule afresh: counted from its own start, a sweep
// would slip by up to a period at every change of leader and never run while
// leaders change faster than that.
type periodBoundaries time.Duration

func (p periodBoundaries) Next(t time.Time) time.Time {
	return t.Truncate(time.Duration(p)).Add(time.Duration(p))
}
