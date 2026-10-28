package riverjobs

import (
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/providerrecovery"
)

// Gate is when a periodic pass is declared.
type Gate int

const (
	Always Gate = iota
	// WithUsageBilling: on hosts that enable usage billing.
	WithUsageBilling
)

// Periodic is one recurring pass, queued by its Args' InsertOpts. Its kind is
// River's periodic id and the name of its lease.
type Periodic struct {
	Args  river.JobArgs
	Every time.Duration
	Gate  Gate
	// PerFleet passes tend the fleet's own state (its Redis): every fleet runs
	// them. The others are leased, so one fleet sharing the database queues
	// each pass.
	PerFleet bool
}

// Periodics are OpenRails' recurring passes.
var Periodics = []Periodic{
	// Engine renewals at their paid-period boundary and due retries: the
	// cadence bounds how long a paying member waits at the boundary.
	{Args: DunningArgs{}, Every: DuePassInterval},
	// Due balance watches, credit lot lapses included.
	{Args: BalanceWatchArgs{}, Every: BalanceWatchInterval},
	{Args: OrderExpiryArgs{}, Every: OrderExpiryInterval},
	{Args: money.UsageOrderArgs{}, Every: money.UsageOrdersInterval, Gate: WithUsageBilling},
	// Overdue rebills with no attempt are probed (NMI) or recorded as missed.
	{Args: RebillWatchArgs{}, Every: RebillWatchInterval},
	{Args: ChargeAttributionArgs{}, Every: ChargeAttributionInterval},
	{Args: DelinquencyArgs{}, Every: DelinquencyInterval},
	// Drift no inline mutation touched: stalled dunning, elapsed grace,
	// abandoned purchases, unmaterialized grant effects.
	{Args: ConvergeSweepArgs{}, Every: ConvergeSweepInterval},
	{Args: NMIHistoryArgs{}, Every: NMIHistoryInterval},
	{Args: CleanupExpiredDataArgs{}, Every: CleanupInterval},
	{Args: StripeWebhookReconcileArgs{}, Every: StripeWebhookReconcileInterval},
	// Each fleet flushes the closed hours of its own Redis's denial counters.
	{Args: AdmissionDenialFlushArgs{}, Every: AdmissionDenialFlushInterval, PerFleet: true},
	// Fans out one refresh job per merchant with a PSP.
	{Args: ProviderRefreshArgs{}, Every: providerrecovery.RefreshInterval},
	// Cards an armed vault must be asked about ahead of their renewal.
	{Args: StoredCardRefreshDueArgs{}, Every: StoredCardRefreshDueInterval},
}

// outstanding are a job's states before it finishes: a pass coalesces with
// one queued, running or waiting to retry.
var outstanding = []rivertype.JobState{
	rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning,
	rivertype.JobStateRetryable, rivertype.JobStateScheduled,
}

// periodPass queues a pass whose retry is the next period's: one attempt, one
// pass per period, so a pass that keeps failing never stacks copies.
func periodPass(every time.Duration) river.InsertOpts {
	return river.InsertOpts{Queue: QueueBilling, MaxAttempts: 1, UniqueOpts: river.UniqueOpts{ByPeriod: every}}
}
