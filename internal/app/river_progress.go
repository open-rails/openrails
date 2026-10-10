package app

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"

	riverjobs "github.com/open-rails/openrails/internal/river"
)

// OpenRails' own answer to "is the cron system progressing?". The detector must
// not depend on what it monitors, so it is not a River job:
// StartRiverProgressMonitor runs a plain goroutine reading River's river_job
// watermarks, which keeps alerting when River never started, runs without
// OpenRails' workers, or is wedged.

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
			DB:            r.DB,
			Pool:          pool,
			RiverSchema:   r.riverSchemaOrDefault(),
			Clock:         clock,
			Registrations: r.workerHealthRegistrations(),
		}
	})
	return r.progress
}

// riverStatsPool returns the pool used to read River's own tables, which live
// in the billing database (Database.RiverSchema).
func (r *Runtime) riverStatsPool() *pgxpool.Pool {
	if r.DB != nil {
		return r.DB.Pool()
	}
	return nil
}

// StartRiverProgressMonitor starts the out-of-River progress detector, once.
// Runtime.Close stops it.
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
	if r != nil && r.riverClosed.Load() {
		return riverjobs.ProgressReport{}, fmt.Errorf("runtime is closed")
	}
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
