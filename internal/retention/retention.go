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
	// UsageIngestWindow is how far back usage may be dated when it is taken
	// in.
	UsageIngestWindow = 35 * Day
	// UsageClockSkew is how far ahead of the server clock usage may be dated.
	UsageClockSkew = 5 * time.Minute

	// MaxSpendWindow is the longest spend window a policy may declare.
	MaxSpendWindow = 31 * Day
)

// Row periods. Where a table refuses ad-hoc deletes, its trigger in the
// baseline declares the same number of days.
const (
	// EventLog is the default retained event lifetime, independent of readers.
	EventLog = 90 * Day
	// SubscriptionTransitions keeps a subscription status change 25 months.
	SubscriptionTransitions = 761 * Day
	// ProviderWrites keeps a finished outbox intent (OutboxIntentTypes) and an
	// operation log entry 25 months.
	ProviderWrites = 761 * Day
	// PaymentAttempts keeps payment attempts, rebill cycles and NMI history
	// months 25 months.
	PaymentAttempts = 761 * Day
	// ExpiredOrderAttempts keeps an order attempt that expired without
	// reaching a provider 90 days past its expiry.
	ExpiredOrderAttempts = 90 * Day
	// UnpaidOrders keeps an unpaid closed order that never started a payment
	// attempt 397 days past its close: the checkout funnel compares a month
	// with the same month a year before.
	UnpaidOrders = 397 * Day
	// MetricCounters keeps a daily webhook-health count 13 months: a month is
	// compared with a year before.
	MetricCounters = 397 * Day
	// OrphanState keeps the deployment-global lease of a periodic kind no
	// fleet has scheduled for 30 days.
	OrphanState = 30 * Day
	// LimitWindows keeps a closed limit window 61 days past its end: the
	// replay horizon, within which late usage and replays still resolve.
	LimitWindows = 61 * Day
	// Leases keeps a gate's lease 61 days past its last sync, so its late
	// reports and replays still resolve.
	Leases = 61 * Day
	// LimitAdjustments keeps a limit adjustment's replay record 61 days.
	LimitAdjustments = 61 * Day
	// Carts keeps a cart 30 days past its last change.
	Carts = 30 * Day
	// ResolvedFindings keeps a resolved reconciliation finding 12 months past
	// its resolution and its last sighting.
	ResolvedFindings = 366 * Day
	// ReleasedParks keeps a released park 90 days past its release.
	ReleasedParks = 90 * Day
	// CustomerMessages keeps a customer message's delivery record two years,
	// the evidence a dispute response cites.
	CustomerMessages = 731 * Day
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

// OutboxIntentTypes are the provider intents that only carried an instruction
// to a provider: once finished, nothing reads them back, and they are deleted
// after ProviderWrites. Every other intent type is the record of money moved
// or refused, a membership enrolled, a card erased or a checkout origin
// registered, and is permanent. The partial index
// provider_intents_updated_at_idx and the cleanup query
// DeleteFinishedOutboxIntentsBefore list the same types; nothing checks them.
var OutboxIntentTypes = []string{
	"nmi_delete_subscription",
	"stripe_cancel_subscription",
	"stripe_resume_subscription",
	"ccbill_cancel_subscription",
	"nmi_payment_method_update",
	"nmi_payment_source_update",
	"nmi_card_vault",
	"network_token",
	"stripe_archive_price",
	"stripe_archive_product",
	"solana_sunset_plan",
	"account_updater",
}
