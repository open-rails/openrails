// Package retention holds how long OpenRails keeps each kind of row. The
// periods are constants: the same for every merchant, and not configuration.
// docs/operations.md lists them; the table comments in the baseline repeat
// each table's class.
package retention

import (
	"math"
	"time"
)

// Day is the unit row periods are counted in. The baseline's delete guards
// carry the same number of days, so a month is never 28 days in one place and
// 31 in another.
const Day = 24 * time.Hour

const (
	// UsageInvoicedMonths is how long a month of usage is kept after the
	// invoice that billed it.
	UsageInvoicedMonths = 24
	// UsageInvoicingLagMonths is how long after a month ends its usage is
	// invoiced at the latest: the longest invoice period is one month.
	UsageInvoicingLagMonths = 1
	// UsageIngestWindow is how far back a usage event may be dated when it is
	// recorded, and how long its idempotency key is honoured after it occurred.
	UsageIngestWindow = 35 * Day
	// UsageClockSkew is how far ahead of the server clock an event may be dated.
	UsageClockSkew = 5 * time.Minute

	// AdmissionMaxWindow is the longest spend window a policy may declare.
	AdmissionMaxWindow = 31 * Day
	// AdmissionMaxHold is the longest a hold may live past its admission.
	AdmissionMaxHold = 30 * Day
	// Admissions is how long an admitted request is kept: the longest spend
	// window that may still count it, plus 30 days.
	Admissions = AdmissionMaxWindow + 30*Day

	// PartitionsAheadMonths is how many months past the current one always
	// have a partition.
	PartitionsAheadMonths = 2
)

// Row periods. Where a table refuses ad-hoc deletes, its trigger in the
// baseline declares the same number of days.
const (
	// SubscriptionTransitions keeps a subscription status change 25 months.
	SubscriptionTransitions = 761 * Day
	// ProviderWrites keeps a finished outbox intent (OutboxIntentTypes) and a
	// mutation log entry 25 months.
	ProviderWrites = 761 * Day
	// PaymentAttempts keeps payment attempts, rebill cycles and NMI history
	// months 25 months.
	PaymentAttempts = 761 * Day
	// ExpiredCheckoutAttempts keeps a checkout attempt that expired without
	// reaching a provider 90 days past its expiry.
	ExpiredCheckoutAttempts = 90 * Day
	// CostObservations keeps a provider cost observation 90 days past the
	// settlement or release of the operation it was read for.
	CostObservations = 90 * Day
	// ResolvedFindings keeps a resolved reconciliation finding 12 months past
	// its resolution and its last sighting.
	ResolvedFindings = 366 * Day
	// ReconciliationRuns keeps a reconciliation run 12 months.
	ReconciliationRuns = 366 * Day
	// NotificationsRead and NotificationsUnread keep a notification 90 days
	// once read, 180 days if never read.
	NotificationsRead   = 90 * Day
	NotificationsUnread = 180 * Day
	// DeliveredHostEvents keeps an acknowledged host event 30 days.
	DeliveredHostEvents = 30 * Day
)

// Days is a period as the whole days the sweep statements take.
func Days(period time.Duration) int32 {
	days := period / Day
	if days < 0 {
		return 0
	}
	if days > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(days)
}

// MonthlyPartitions is a table partitioned by month on Key. A partition is
// dropped once its whole range is at or before DropBefore(now). Partitions are
// created from the month a row may still be written into (WriteBack ago)
// through PartitionsAheadMonths past the current month; older months exist
// because they were once current.
type MonthlyPartitions struct {
	Table      string
	Key        string
	WriteBack  time.Duration
	DropBefore func(now time.Time) time.Time
}

// Partitioned lists the partitioned tables.
var Partitioned = []MonthlyPartitions{
	{Table: "usage_events", Key: "occurred_at", WriteBack: UsageIngestWindow, DropBefore: UsageDropBefore},
	{Table: "admission_operations", Key: "admitted_at", DropBefore: AdmissionsDropBefore},
}

// MonthStart is the first instant of t's UTC month.
func MonthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// UsageDropBefore is a month boundary: a usage month goes when the month
// after it, by whose end it was invoiced, ended UsageInvoicedMonths ago.
func UsageDropBefore(now time.Time) time.Time {
	return MonthStart(now).AddDate(0, -(UsageInvoicedMonths + UsageInvoicingLagMonths), 0)
}

// AdmissionsDropBefore is the oldest admission still kept.
func AdmissionsDropBefore(now time.Time) time.Time { return now.UTC().Add(-Admissions) }

// Range is the span of months a row may be written into at now, plus the
// months ahead.
func (p MonthlyPartitions) Range(now time.Time) (from, through time.Time) {
	return MonthStart(now.Add(-p.WriteBack)), MonthStart(now).AddDate(0, PartitionsAheadMonths, 0)
}

// RetainedRange is the span of every month still kept at now: what a restore
// of archived rows may need.
func (p MonthlyPartitions) RetainedRange(now time.Time) (from, through time.Time) {
	_, through = p.Range(now)
	return MonthStart(p.DropBefore(now)), through
}

// OutboxIntentTypes are the provider intents that only carried an instruction
// to a provider: once finished, nothing reads them back, and they are deleted
// after ProviderWrites. Every other intent type is the record of money moved
// or refused, a membership enrolled or a card erased, and is permanent. The
// baseline's idx_provider_intents_finished_outbox lists the same types.
var OutboxIntentTypes = []string{
	"nmi_delete_subscription",
	"stripe_cancel_subscription",
	"ccbill_cancel_subscription",
	"nmi_payment_method_update",
	"nmi_payment_source_update",
	"nmi_card_vault",
	"network_token",
	"stripe_archive_price",
	"stripe_archive_product",
	"solana_sunset_plan",
	"bt_account_updater_batch",
}
