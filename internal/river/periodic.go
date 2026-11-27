package riverjobs

import (
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// Periodic is one recurring pass, queued by its Args' InsertOpts. Its kind is
// River's periodic id and the name of its lease.
type Periodic struct {
	Args  river.JobArgs
	Every time.Duration
	// Hourly runs the pass at the top of each UTC hour instead of Every after
	// the last one.
	Hourly bool
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
	// MRR risk changes queued while no drain was woken.
	{Args: MRRRiskArgs{}, Every: MRRRiskInterval},
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
