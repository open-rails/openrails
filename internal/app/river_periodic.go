package app

import (
	"context"
	"time"

	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/logx"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// periodicJob declares p with River. RunOnStart catches up an overdue pass;
// for an interval the shared helper's retained-history check skips a recent
// one after a restart, and an hourly pass's insert skips a second in its hour.
// A pass is queued only by the fleet holding its lease.
func (r *Runtime) periodicJob(cfg *river.Config, p riverjobs.Periodic) *river.PeriodicJob {
	id := p.Args.Kind()
	construct := func() (river.JobArgs, *river.InsertOpts) {
		if !r.claimPeriodic(id, p.Every) {
			return nil, nil
		}
		return p.Args, nil
	}
	opts := &river.PeriodicJobOpts{ID: id, RunOnStart: true}
	if p.Hourly {
		return river.NewPeriodicJob(riverjobs.TopOfHour, construct, opts)
	}
	return riverhelpers.NewPeriodicJob(cfg, p.Every, construct, opts)
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
		r.Log.WarnContext(ctx, "periodic lease unreadable; running the pass", logx.Err(err), "periodic", id)
		return true
	}
	if n == 0 {
		r.Log.DebugContext(ctx, "periodic pass leased to another fleet; skipped", "periodic", id, "fleet", holder)
		return false
	}
	return true
}
