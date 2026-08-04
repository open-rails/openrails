package app

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/config"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// #895: OpenRails' own liveness answer for "is the cron system progressing?".
//
// The detector must not require the monitored subsystem to be alive, so it is
// NOT a River job: StartRiverProgressMonitor runs an ordinary goroutine that
// reads River's own river_job watermarks. It keeps reporting — and alerting —
// when River was never started, was started without OpenRails' workers, or is
// wedged.

// riverProgressMonitor lazily builds the monitor. It is safe to call before or
// after a River client exists; the monitor reads tables, not clients.
func (r *Runtime) riverProgressMonitor() *riverjobs.ProgressMonitor {
	r.progressOnce.Do(func() {
		clock := r.Clock
		if clock == nil {
			clock = clockwork.NewRealClock()
		}
		pool := r.riverStatsPool()
		r.progress = &riverjobs.ProgressMonitor{
			DB:                  r.DB,
			Pool:                pool,
			RiverSchema:         config.RiverSchema,
			Clock:               clock,
			Registrations:       r.workerHealthRegistrations(),
			NotificationService: r.NotificationService,
		}
	})
	return r.progress
}

// riverStatsPool returns the pool used to read River's own tables. River lives
// in `public` in the SAME database as the billing schema (#545), so the app
// pool reads it directly; a dedicated River pool is used when one exists.
func (r *Runtime) riverStatsPool() *pgxpool.Pool {
	if r.riverPool != nil {
		return r.riverPool
	}
	if r.DB != nil {
		return r.DB.Pool()
	}
	return nil
}

// StartRiverProgressMonitor starts the out-of-River progress detector, once.
// The returned stop function is idempotent; Runtime.Close calls it.
func (r *Runtime) StartRiverProgressMonitor(ctx context.Context) {
	if r == nil || r.DB == nil {
		return
	}
	r.progressStartOnce.Do(func() {
		monitorCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		r.progressCancel = cancel
		r.progressDone = make(chan struct{})
		monitor := r.riverProgressMonitor()
		go func() {
			defer close(r.progressDone)
			monitor.Run(monitorCtx)
		}()
	})
}

// stopRiverProgressMonitor cancels the monitor goroutine and waits for it.
func (r *Runtime) stopRiverProgressMonitor() {
	if r == nil {
		return
	}
	r.progressStopOnce.Do(func() {
		if r.progressCancel != nil {
			r.progressCancel()
		}
		if r.progressDone != nil {
			<-r.progressDone
		}
	})
}

// RiverProgress evaluates the periodic fleet RIGHT NOW and returns the report.
// It is a pure read — no job runs, nothing is enqueued — so a host may call it
// from its own health endpoint and get a truthful answer while River is down.
func (r *Runtime) RiverProgress(ctx context.Context) (riverjobs.ProgressReport, error) {
	if r == nil || r.DB == nil {
		return riverjobs.ProgressReport{}, fmt.Errorf("river progress: runtime not initialized")
	}
	return r.riverProgressMonitor().Check(ctx)
}

// progressLifecycle groups the monitor's once-guards so Runtime's struct stays
// readable; embedded into Runtime.
type progressLifecycle struct {
	progress          *riverjobs.ProgressMonitor
	progressOnce      sync.Once
	progressStartOnce sync.Once
	progressStopOnce  sync.Once
	progressCancel    context.CancelFunc
	progressDone      chan struct{}
}
