package riverjobs

import (
	"slices"
	"time"

	"github.com/open-rails/openrails/internal/disputejob"

	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/internal/balancelook"

	"github.com/open-rails/openrails/internal/billingfact"
	"github.com/open-rails/openrails/internal/convergejob"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/intents/wake"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/alerting"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/usageerasure"
)

// DefaultTimeout is an OpenRails job's budget: River cancels the job past it,
// and its rescuer returns a crashed one to work no sooner.
const DefaultTimeout = 10 * time.Minute

// RescueAfter is River's RescueStuckJobsAfter when the host sets none. A
// crashed job is rescued once it has run past both this and its timeout, so
// a long pass is never rescued while it runs inside its budget.
const RescueAfter = 10 * time.Minute

// kinds are OpenRails' job kinds and their timeouts. Provider refresh gets an
// hour, as does NMI history, PSP import gets two hours, and the billing pass
// uses its scan budgets.
var kinds = map[string]time.Duration{
	balancelook.Kind:                     DefaultTimeout,
	usageerasure.Kind:                    DefaultTimeout,
	money.KindUsageInvoices:              DefaultTimeout,
	KindSubscriptionConverge:             DefaultTimeout,
	KindProviderObjectObserve:            DefaultTimeout,
	convergejob.Kind:                     DefaultTimeout,
	disputejob.MonitorKind:               DefaultTimeout,
	disputejob.ResponseKind:              DefaultTimeout,
	alerting.DeliveryKind:                DefaultTimeout,
	billingfact.SendKind:                 DefaultTimeout,
	wake.Kind:                            DefaultTimeout,
	paymentmethods.KindStoredCardRefresh: DefaultTimeout,
	merchants.SecretCleanupArgs{}.Kind(): DefaultTimeout,
	merchants.UsagePurgeArgs{}.Kind():    DefaultTimeout,
	destructive.DrainErasuresKind:        DefaultTimeout,
	subscriptions.KindPriceMigration:     DefaultTimeout,
	KindCleanupExpiredData:               time.Hour,
	KindProviderRefreshRequested:         time.Hour,
	KindPacedProviderRefresh:             time.Hour,
	KindMerchantHealth:                   time.Hour,
	KindNMIHistory:                       time.Hour,
	reconcile.KindChargeAttributions:     time.Hour,
	KindStripeWebhookReconcile:           DefaultTimeout,
	KindMRRRisk:                          DefaultTimeout,
	KindBillingPass:                      BillingPassTimeout(),
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
