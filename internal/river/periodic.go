package riverjobs

import (
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/openrails/internal/providerrecovery"
)

// Gate is when a periodic pass is declared.
type Gate int

const (
	Always Gate = iota
	// WithUsageBilling: on hosts that enable usage billing.
	WithUsageBilling
	// WithRedis: on fleets with their own Redis.
	WithRedis
)

// Periodic is one recurring pass, queued by its Args' InsertOpts. Its kind is
// River's periodic id and the name of its lease.
type Periodic struct {
	Args  river.JobArgs
	Every time.Duration
	// Hourly runs the pass at the top of each UTC hour instead of Every after
	// the last one.
	Hourly bool
	Gate   Gate
	// PerFleet passes tend the fleet's own state (its Redis): every fleet runs
	// them. The others are leased, so one fleet sharing the database queues
	// each pass.
	PerFleet bool
}

// Periodics are OpenRails' recurring passes.
var Periodics = []Periodic{
	// Every due scan, in turn: what is due by the pass is billed, retried,
	// lapsed or expired by it.
	{Args: BillingPassArgs{}, Every: BillingPassInterval, Hourly: true},
	// Health findings and timed access lapses, for merchants with activity.
	{Args: MerchantHealthArgs{}, Every: MerchantHealthInterval},
	{Args: CleanupExpiredDataArgs{}, Every: CleanupInterval},
	// Queues every Stripe PSP's endpoint pass: Stripe disables or deletes an
	// endpoint without telling us.
	{Args: StripeWebhookReconcileArgs{}, Every: StripeWebhookReconcileInterval},
	// Each fleet flushes the closed hours of its own Redis's denial counters.
	{Args: AdmissionDenialFlushArgs{}, Every: AdmissionDenialFlushInterval, Gate: WithRedis, PerFleet: true},
	// Fans out one refresh job per merchant with a PSP.
	{Args: ProviderRefreshArgs{}, Every: providerrecovery.RefreshInterval},
}

// topOfHour is the cron schedule 0 * * * * in UTC.
type topOfHour struct{}

func (topOfHour) Next(t time.Time) time.Time { return t.UTC().Truncate(time.Hour).Add(time.Hour) }

// TopOfHour schedules a pass at the start of each UTC hour.
var TopOfHour river.PeriodicSchedule = topOfHour{}

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

// coalescedPass queues a pass whose retry is the next one: one attempt, and an
// insert while a pass is queued or running joins it, so passes never stack.
func coalescedPass() river.InsertOpts {
	return river.InsertOpts{Queue: QueueBilling, MaxAttempts: 1, UniqueOpts: river.UniqueOpts{ByState: outstanding}}
}
