package riverjobs

import (
	"slices"
	"time"

	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/internal/intents/wake"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/reconcile"
)

// DefaultTimeout is an OpenRails job's budget: River cancels the job past it,
// and its rescuer returns a crashed one to work no sooner.
const DefaultTimeout = 10 * time.Minute

// RescueAfter is River's RescueStuckJobsAfter when the host sets none. A
// crashed job is rescued once it has run past both this and its timeout, so
// a long pass is never rescued while it runs inside its budget.
const RescueAfter = 10 * time.Minute

// kinds are OpenRails' job kinds and their timeouts: an hour for the passes
// that read every merchant's providers, and for the stored-card pass, whose
// own budget is 10 minutes; two for a PSP import.
var kinds = map[string]time.Duration{
	KindDunning:                          DefaultTimeout,
	KindBalanceWatch:                     DefaultTimeout,
	KindOrderExpiry:                      DefaultTimeout,
	money.KindUsageOrders:                DefaultTimeout,
	KindDelinquency:                      DefaultTimeout,
	KindAdmissionDenialFlush:             DefaultTimeout,
	KindProviderRefresh:                  DefaultTimeout,
	KindSubscriptionConverge:             DefaultTimeout,
	KindProviderObjectObserve:            DefaultTimeout,
	DestructiveAlertArgs{}.Kind():        DefaultTimeout,
	wake.Kind:                            DefaultTimeout,
	paymentmethods.KindStoredCardRefresh: DefaultTimeout,
	merchants.SecretCleanupArgs{}.Kind(): DefaultTimeout,
	KindStoredCardRefreshDue:             time.Hour,
	KindCleanupExpiredData:               time.Hour,
	KindProviderRefreshMerchant:          time.Hour,
	KindConvergeSweep:                    time.Hour,
	KindRebillWatch:                      time.Hour,
	reconcile.KindChargeAttributions:     time.Hour,
	KindNMIHistory:                       time.Hour,
	KindStripeWebhookReconcile:           DefaultTimeout,
	KindConfigRealerts:                   DefaultTimeout,
	KindPSPImport:                        2 * time.Hour,
}

// Timeout is kind's budget; zero for a kind that is not OpenRails'.
func Timeout(kind string) time.Duration { return kinds[kind] }

// Kinds are OpenRails' job kinds, sorted.
func Kinds() []string {
	out := make([]string, 0, len(kinds))
	for kind := range kinds {
		out = append(out, kind)
	}
	slices.Sort(out)
	return out
}

// WorkerDefaults is River's, with the kind's budget as the job's timeout:
// the client's JobTimeout is the host's, for its own workers.
type WorkerDefaults[T river.JobArgs] struct{ river.WorkerDefaults[T] }

func (WorkerDefaults[T]) Timeout(*river.Job[T]) time.Duration {
	var args T
	return Timeout(args.Kind())
}
