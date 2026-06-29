package subscriptions

import (
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
)

// Cadence-relative dunning (#359).
//
// The dunning schedule is a HARDCODED function of the price's billing cycle —
// there is no configuration knob. Retrying a failed $5/day rebill for two
// weeks makes no sense, and neither does giving a $100/year sub only 3 days;
// the cadence of recovery follows the cadence of payment.
//
// The schedule is a list of retry OFFSETS from the INITIAL failure (not a
// fixed interval — the monthly schedule is progressive, front-loading retries
// where transient declines clear):
//
//	cycle < 4 days        -> no retries (the first failure is terminal)
//	4 days <= cycle < 28  -> +1d, +2d                  ("weekly": 3 failures total)
//	cycle >= 28 days      -> +2d, +5d, +9d, +13d       ("monthly": 5 failures total
//	                         over ~2 weeks; capped — yearly is never more
//	                         generous than monthly)
//	unknown (<= 0)        -> monthly schedule, defensively (callers log; a
//	                         past_due one-time-price subscription shouldn't exist)
//
// Example monthly timeline: fail June 1 -> retries June 3, 6, 10, 14 -> the
// June 14 decline (5th total failure) is terminal.
//
// Boundary rationale — the binding principle is that the retry span (and the
// derived staleness window) must stay WELL INSIDE one billing cycle: a weekly
// sub must not still be dunning the old period when the next week's charge is
// due.
//
//   - The weekly tier's derived window is 3d (last offset 2d + one day of
//     slack). For that to fit inside one cycle the cycle must be >= 4 days, so
//     cycles of 1-3 days get no retries at all (a 2-day sub retried daily
//     would still be dunning when the next period is due).
//   - The monthly tier's derived window is 14d (last offset 13d + slack). The
//     tier starts at 28 days so 4-weekly "monthly" billing gets the monthly
//     schedule while the retry span (13d) stays at ~half a cycle; for any
//     shorter cycle (Paul's sketch had this tier start at ~10d) the 14d window
//     would outlast the cycle itself.
//
// Retry spans against the principle: weekly tier 2d (50% of the shortest 4d
// cycle, 29% of a true week), monthly tier 13d (46% of the shortest 28d cycle,
// 43% of a month).
const (
	// dunningMinRetryCycleDays is the shortest billing cycle that gets any
	// retries at all; below it the first failure is terminal.
	dunningMinRetryCycleDays = 4
	// dunningMonthlyCycleDays is where the monthly (capped) tier starts.
	dunningMonthlyCycleDays = 28
	// dunningWindowSlack is added past the last retry offset when deriving the
	// staleness window: one day tolerates a late worker run without
	// prematurely window-cancelling a sub that is mid-schedule.
	dunningWindowSlack = 24 * time.Hour
)

// DeclineCategory classifies a rail decline for dunning purposes.
type DeclineCategory int

const (
	// DeclineSoft indicates a transient/recoverable decline (e.g. insufficient
	// funds). Dunning should keep retrying on the normal schedule.
	DeclineSoft DeclineCategory = iota
	// DeclineHard indicates a permanent decline (e.g. stolen card, do-not-honor,
	// account closed). Dunning must stop immediately and cancel the subscription;
	// retrying cannot succeed and may flag the merchant with the card networks.
	DeclineHard
)

// hardDeclineNMICodes maps NMI Payment API response_code values that represent
// permanent declines where further retries must stop immediately. These are
// card/account-level failures that cannot be cured by retrying the same stored
// payment method, so we cancel rather than burn dunning attempts (and risk
// flagging the merchant with the card networks).
//
//	201 = Do not honor
//	204 = Transaction not allowed
//	220 = Incorrect payment information
//	221 = No such card issuer
//	222 = No card number on file with issuer
//	223 = Expired card
//	224 = Invalid expiration date
//	225 = Invalid card security code
//	226 = Invalid PIN
//	240 = Call issuer for further information
//	250 = Pick up card
//	251 = Lost card
//	252 = Stolen card
//	253 = Fraudulent card
//	261 = Declined - Stop all recurring payments (account closed)
//	262 = Declined - Stop this recurring program
//	263 = Declined - Update cardholder data available (stored data no longer valid)
//	461 = Unsupported card type
//
// Soft (retryable) codes are intentionally NOT listed and fall through to the
// normal retry schedule:
//
//	200 = Declined by rail (generic)   202 = Insufficient funds
//	203 = Over limit                         260 = Declined w/ further instructions
//	264 = Declined - Retry in a few days     300 = Rejected by gateway
//	400 = Rail error                    410/411 = Merchant config/inactive
//	420/421 = Communication error            430 = Duplicate transaction
//	440/441 = Rail format/info error    460 = Rail feature unavailable
var hardDeclineNMICodes = map[int]bool{
	201: true,
	204: true,
	220: true,
	221: true,
	222: true,
	223: true,
	224: true,
	225: true,
	226: true,
	240: true,
	250: true,
	251: true,
	252: true,
	253: true,
	261: true,
	262: true,
	263: true,
	461: true,
}

// ClassifyNMIDecline categorizes an NMI Payment API response_code as a hard or
// soft decline. Unknown/zero codes and known transient codes (e.g. 200 generic
// decline, 202 insufficient funds, 264 retry-in-a-few-days, 300/400/420
// gateway/rail/comms errors) are treated as soft so the normal retry
// schedule applies. Merchant-side errors (410/411) are also soft so a
// configuration problem on our end does not cancel the customer's subscription.
func ClassifyNMIDecline(responseCode int) DeclineCategory {
	if hardDeclineNMICodes[responseCode] {
		return DeclineHard
	}
	return DeclineSoft
}

var (
	// dunningOffsetsWeekly: 2 retries, one day apart.
	dunningOffsetsWeekly = []time.Duration{24 * time.Hour, 48 * time.Hour}
	// dunningOffsetsMonthly: progressive +2d, +5d, +9d, +13d (gaps 2,3,4,4).
	dunningOffsetsMonthly = []time.Duration{
		2 * 24 * time.Hour,
		5 * 24 * time.Hour,
		9 * 24 * time.Hour,
		13 * 24 * time.Hour,
	}
)

// DunningRetryOffsets returns the hardcoded retry schedule for a billing cycle
// expressed in days: each entry is a retry moment as an offset from the
// INITIAL failure. An empty schedule means no retries — the first failure is
// terminal.
//
// billingCycleDays <= 0 means the cycle is unknown (e.g. a one-time price);
// the monthly schedule is returned defensively and callers should log.
//
// Callers must not mutate the returned slice.
func DunningRetryOffsets(billingCycleDays int) []time.Duration {
	switch {
	case billingCycleDays <= 0:
		// Unknown cycle: monthly fallback (callers log).
		return dunningOffsetsMonthly
	case billingCycleDays < dunningMinRetryCycleDays:
		// Daily-ish cadence: no dunning, first failure is terminal.
		return nil
	case billingCycleDays < dunningMonthlyCycleDays:
		// Weekly-ish cadence.
		return dunningOffsetsWeekly
	default:
		// Monthly and longer (capped: yearly == monthly).
		return dunningOffsetsMonthly
	}
}

// DunningMaxFailures returns how many consecutive rebill failures (counting
// the initial one) a billing cycle tolerates before the subscription is
// terminally cancelled: one per retry offset, plus the initial failure.
// A 0-retry cycle returns 1 — the first failure is terminal.
func DunningMaxFailures(billingCycleDays int) int {
	return len(DunningRetryOffsets(billingCycleDays)) + 1
}

// DunningNextRetryIn returns how long after the failures-th consecutive
// failure (1-based, counting the initial failure) the next retry should run,
// or 0 when that failure is terminal (no retry follows).
//
// The gaps reproduce the offset schedule exactly when each retry runs on time
// (monthly: 2d, 3d, 4d, 4d) and degrade gracefully when the worker is late —
// the next retry is always scheduled relative to the failure that just
// happened, never into the past.
func DunningNextRetryIn(billingCycleDays, failures int) time.Duration {
	offsets := DunningRetryOffsets(billingCycleDays)
	if failures < 1 || failures > len(offsets) {
		return 0
	}
	if failures == 1 {
		return offsets[0]
	}
	return offsets[failures-1] - offsets[failures-2]
}

// DunningWindow returns the DERIVED staleness window (#344, #359): how long
// past the missed rebill (current_period_ends_at) dunning may still attempt a
// charge. Anything older is cancelled + downgraded WITHOUT a charge — a card
// that failed months ago is never surprise-charged by a catch-up run.
//
// window = last retry offset + one day of slack (14d for monthly cycles, 3d
// for weekly). A 0-retry cycle derives a ZERO window: any missed rebill is
// immediately terminal.
func DunningWindow(billingCycleDays int) time.Duration {
	offsets := DunningRetryOffsets(billingCycleDays)
	if len(offsets) == 0 {
		return 0
	}
	return offsets[len(offsets)-1] + dunningWindowSlack
}

// BillingCycleDaysOf returns the price's billing cycle in days, or 0 when the
// price or its cycle is unknown (one-time prices) — the defensive
// monthly-fallback input to the Dunning* schedule functions.
func BillingCycleDaysOf(price *models.Price) int {
	cycleDays := price.RecurringCycleDays()
	if cycleDays == nil {
		return 0
	}
	return *cycleDays
}

// Renewal grace windows (#368).
//
// Silence at period end (no renewal webhook either way) no longer cuts access
// at the period-end second: activation and every renewal pre-append a bounded,
// revocable `grace` entitlement window trailing the paid window, so a missed
// success webhook, a provider billing on its own day boundary, or a merely
// late webhook never gates a paying user. Paid windows stay truthful (end_at
// = period end, immutable); generosity is a SEPARATE window that the #367
// liveness sync (or any renewal/terminal resolution) revokes the moment truth
// arrives. NOT a config knob — cadence-derived and hardcoded, like the
// dunning schedule.
const (
	// graceSlackCap bounds the trailing grace window: at most 48h of silence
	// generosity (Paul, 2026-06-12). This grace exists for provider-side
	// schedule rounding and late webhooks — NOT for dunning, which has its
	// own rail-driven grace; anything a rounding error can't explain
	// within two days is the #367 prober's and dunning's problem.
	graceSlackCap = 48 * time.Hour
	// LivenessProbeSlack is how long past current_period_ends_at a silent
	// active subscription must be before the #367 liveness sync treats it as
	// part of the silent-lapsed cohort. One day tolerates provider-side day
	// boundaries and late webhooks before any provider probe fires.
	LivenessProbeSlack = 24 * time.Hour
)

// GraceSlack returns the duration of the trailing renewal grace window for a
// billing cycle expressed in days: half the billing cycle, capped at 48h
// (daily cycles get 12h, 3-day 36h, 4-day-and-longer incl. monthly 48h).
// billingCycleDays <= 0
// means the cycle is unknown (one-time price); the monthly cap is returned
// defensively to match the Dunning* fallbacks.
func GraceSlack(billingCycleDays int) time.Duration {
	if billingCycleDays <= 0 {
		return graceSlackCap
	}
	half := time.Duration(billingCycleDays) * 12 * time.Hour
	if half > graceSlackCap {
		return graceSlackCap
	}
	return half
}

// RenewalGraceEligibleRail reports whether a rail's subscriptions
// get the pre-appended renewal grace window (#368): NMI-backed + Stripe.
// CCBill keeps its own retry-driven grace (the webhook handler appends grace
// from CCBill's nextRetryDate); Solana is pull-based — OpenRails is the only
// puller, so there is no webhook silence to bridge.
func RenewalGraceEligibleRail(rail models.Rail) bool {
	return rails.IsNMIBackedRail(rail) || rail == models.RailStripe
}
