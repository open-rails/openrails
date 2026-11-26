package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
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

// JobProgress is the openrails_job_progress verdict, a pure read of River's
// tables and the billing pass's lease: the fleet's River has a leader, no
// OpenRails job runs past its timeout, and within a day and the pass's
// timeout some fleet queued a billing pass (the fleet that claims its lease
// queues it; counted from the leader's election, which queues one at once),
// each pass this fleet queued ran to its end, and its last found no critical
// work a day overdue unless another fleet has queued one since.
func (r *Runtime) JobProgress(ctx context.Context) error {
	h, err := r.JobHealth(ctx)
	if err != nil {
		return err
	}
	schema := r.riverSchemaOrDefault()
	if h.LeaderElectedAt == nil {
		return fmt.Errorf("no River leader in schema %q: periodic passes and crash rescue are not running", schema)
	}
	var stalled []string
	for _, k := range h.Kinds {
		if k.Overrunning > 0 {
			stalled = append(stalled, fmt.Sprintf("%s: %d running past its %s timeout", k.Kind, k.Overrunning, riverjobs.Timeout(k.Kind)))
		}
	}
	claim, err := r.DB.GenDirectory().GetPeriodicLeaseClaim(ctx, riverjobs.KindBillingPass)
	claimed := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("job health: billing pass lease: %w", err)
	}
	window := riverjobs.CriticalOverdue + riverjobs.Timeout(riverjobs.KindBillingPass)
	queued := *h.LeaderElectedAt
	if claimed && claim.ClaimedAt.After(queued) {
		queued = claim.ClaimedAt
	}
	if h.CheckedAt.Sub(queued) > window {
		stalled = append(stalled, fmt.Sprintf("no fleet has queued %s since %s", riverjobs.KindBillingPass, queued.UTC().Format(time.RFC3339)))
	}
	if q := h.PassQueuedAt; q != nil && h.CheckedAt.Sub(*q) > window {
		stalled = append(stalled, fmt.Sprintf("%s queued at %s has not run to its end", riverjobs.KindBillingPass, q.UTC().Format(time.RFC3339)))
	}
	if h.LastPass != nil && len(h.LastPass.Overdue) > 0 && (!claimed || claim.Holder == schema) {
		stalled = append(stalled, fmt.Sprintf("a day overdue at %s: %s", h.LastPass.FinishedAt.UTC().Format(time.RFC3339), strings.Join(h.LastPass.Overdue, ", ")))
	}
	if len(stalled) > 0 {
		return errors.New("OpenRails jobs are not progressing: " + strings.Join(stalled, "; "))
	}
	return nil
}
