package app

import (
	"context"
	"time"

	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db/gen"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// periodicJob declares p with River. RunOnStart catches up an overdue pass;
// the shared helper's retained-history check skips a recent one after a
// restart. A leased pass is queued only by the fleet holding its lease.
func (r *Runtime) periodicJob(cfg *river.Config, p riverjobs.Periodic) *river.PeriodicJob {
	id := p.Args.Kind()
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
