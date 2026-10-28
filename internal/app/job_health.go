package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// JobHealth reads OpenRails' jobs from this fleet's River tables.
func (r *Runtime) JobHealth(ctx context.Context) (riverjobs.Health, error) {
	if r == nil || r.DB == nil {
		return riverjobs.Health{}, fmt.Errorf("job health: runtime not initialized")
	}
	if r.riverClosed.Load() {
		return riverjobs.Health{}, fmt.Errorf("runtime is closed")
	}
	return riverjobs.ReadHealth(ctx, r.DB.Pool(), r.riverSchemaOrDefault())
}

// ListWorkerHealth is each OpenRails job kind in this fleet's River tables.
func (r *Runtime) ListWorkerHealth(ctx context.Context) ([]billing.WorkerHealth, error) {
	h, err := r.JobHealth(ctx)
	return h.Kinds, err
}

// JobProgress is the openrails_job_progress verdict, a pure read: the fleet's
// River has a leader, no OpenRails job runs past its timeout, and each
// periodic pass this fleet queues completed within two periods and its
// timeout (counted from the leader's election, which queues an overdue pass
// at once). A pass whose live lease another fleet holds is that fleet's.
func (r *Runtime) JobProgress(ctx context.Context) error {
	h, err := r.JobHealth(ctx)
	if err != nil {
		return err
	}
	schema := r.riverSchemaOrDefault()
	if h.LeaderElectedAt == nil {
		return fmt.Errorf("no River leader in schema %q: periodic passes and crash rescue are not running", schema)
	}
	var leased []string
	for _, p := range riverjobs.Periodics {
		if !p.PerFleet {
			leased = append(leased, p.Args.Kind())
		}
	}
	elsewhere, err := r.DB.GenDirectory().ListPeriodicLeasesHeldElsewhere(ctx, gen.ListPeriodicLeasesHeldElsewhereParams{Kinds: leased, Holder: schema})
	if err != nil {
		return fmt.Errorf("job health: periodic leases: %w", err)
	}
	byKind := map[string]billing.WorkerHealth{}
	var stalled []string
	for _, k := range h.Kinds {
		byKind[k.Kind] = k
		if k.Overrunning > 0 {
			stalled = append(stalled, fmt.Sprintf("%s has %d jobs running past their %s timeout", k.Kind, k.Overrunning, riverjobs.Timeout(k.Kind)))
		}
	}
	for _, p := range riverjobs.Periodics {
		kind := p.Args.Kind()
		if !r.declares(p.Gate) || (!p.PerFleet && slices.Contains(elsewhere, kind)) {
			continue
		}
		since := *h.LeaderElectedAt
		if last := byKind[kind].LastCompletedAt; last != nil && last.After(since) {
			since = *last
		}
		if h.CheckedAt.Sub(since) > 2*p.Every+riverjobs.Timeout(kind) {
			stalled = append(stalled, fmt.Sprintf("%s has not completed since %s", kind, since.UTC().Format(time.RFC3339)))
		}
	}
	if len(stalled) > 0 {
		return errors.New("OpenRails jobs are not progressing: " + strings.Join(stalled, "; "))
	}
	return nil
}
