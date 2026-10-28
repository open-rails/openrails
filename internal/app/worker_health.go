package app

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
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

// Timeout is the inner worker's: its kind's budget.
func (w *healthTrackedWorker[T]) Timeout(job *river.Job[T]) time.Duration {
	return w.inner.Timeout(job)
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
	if got, want := worker.Timeout(&river.Job[T]{JobRow: &rivertype.JobRow{Kind: args.Kind()}}), riverjobs.Timeout(args.Kind()); got != want {
		return fmt.Errorf("%s: timeout %s is not its kind's budget %s", args.Kind(), got, want)
	}
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

// healthPeriodic registers a restart-safe interval, recording
// kind -> interval (shortest wins) for the staleness alert rule. A periodic's
// River ID is its job kind unless opts names one, so a River client can turn
// a single periodic off (PeriodicJobs().RemoveByID). The pass is queued only
// by the fleet holding the kind's lease (see claimPeriodic).
func (r *Runtime) healthPeriodic(cfg *river.Config, interval time.Duration, ctor river.PeriodicJobConstructor, opts *river.PeriodicJobOpts) *river.PeriodicJob {
	return r.periodic(cfg, true, interval, ctor, opts)
}

// fleetPeriodic is a periodic over this fleet's own state (its River jobs, its
// Redis): every fleet runs it.
func (r *Runtime) fleetPeriodic(cfg *river.Config, interval time.Duration, ctor river.PeriodicJobConstructor, opts *river.PeriodicJobOpts) *river.PeriodicJob {
	return r.periodic(cfg, false, interval, ctor, opts)
}

func (r *Runtime) periodic(cfg *river.Config, leased bool, interval time.Duration, ctor river.PeriodicJobConstructor, opts *river.PeriodicJobOpts) *river.PeriodicJob {
	own := river.PeriodicJobOpts{}
	if opts != nil {
		own = *opts
	}
	// RunOnStart asks the shared helper to catch up due or never-seen work.
	// Its retained-history check suppresses recent runs after a restart.
	own.RunOnStart = true
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
	return riverhelpers.NewPeriodicJob(cfg, interval, ctor, &own)
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

// PeriodicJobIDs are the River IDs of the periodic jobs this runtime gave its
// River client.
func (r *Runtime) PeriodicJobIDs() []string { return slices.Clone(r.periodicIDs) }
