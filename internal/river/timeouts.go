package riverjobs

import (
	"time"

	"github.com/riverqueue/river"
)

// DefaultTimeout is an OpenRails job's budget: River cancels the job past it,
// and its rescuer returns a crashed one to work no sooner.
const DefaultTimeout = 10 * time.Minute

// RescueAfter is River's RescueStuckJobsAfter when the host sets none. A
// crashed job is rescued once it has run past both this and its timeout, so
// a long pass is never rescued while it runs inside its budget.
const RescueAfter = 10 * time.Minute

// longTimeouts are the passes that read every merchant's providers, or a
// whole PSP, and the stored-card pass, whose own budget is 10 minutes.
var longTimeouts = map[string]time.Duration{
	KindStoredCardRefreshDue:    time.Hour,
	KindCleanupExpiredData:      time.Hour,
	KindProviderRefreshMerchant: time.Hour,
	KindConvergeSweep:           time.Hour,
	KindRebillWatch:             time.Hour,
	KindChargeAttributions:      time.Hour,
	KindNMIHistory:              time.Hour,
	KindStripeWebhookReconcile:  time.Hour,
	KindSolanaCrank:             time.Hour,
	KindPSPImport:               2 * time.Hour,
}

// Timeout is kind's budget.
func Timeout(kind string) time.Duration {
	if d, ok := longTimeouts[kind]; ok {
		return d
	}
	return DefaultTimeout
}

// WorkerDefaults is River's, with the kind's budget as the job's timeout:
// the client's JobTimeout is the host's, for its own workers.
type WorkerDefaults[T river.JobArgs] struct{ river.WorkerDefaults[T] }

func (WorkerDefaults[T]) Timeout(*river.Job[T]) time.Duration {
	var args T
	return Timeout(args.Kind())
}
