package collection

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
)

const day = 24 * time.Hour

// The bucket tables are cancellation policy: every row is pinned in both
// directions so an added, removed or moved row fails here.
func TestDeclineTablesArePinned(t *testing.T) {
	fix, dead := DeclineFixPaymentMethod, DeclineNonRecoverable

	nmiRetry := []int{100, 200, 202, 203, 260, 264, 300, 400, 410, 411, 420, 421, 430, 440, 441, 460}
	nmi := map[int]DeclineOutcome{
		201: fix, 204: fix, 220: fix, 221: fix, 222: fix, 223: fix, 224: fix, 225: fix,
		226: fix, 240: fix, 250: fix, 251: fix, 263: fix, 461: fix,
		252: dead, 253: dead, 261: dead, 262: dead,
	}
	pinTable(t, "nmi", nmi, nmiDeclineOutcomes, strconv.Itoa)
	for code, want := range nmi {
		require.Equal(t, want, ClassifyNMIResponseCode(code), "nmi %d", code)
		id := nmiDeclineLocalizationIDs[code]
		require.NotEmpty(t, id, "nmi %d has no localization id; its string form would fall to retry", code)
		for _, shape := range []string{id, "nmi_" + id, "nmi_response_" + strconv.Itoa(code)} {
			require.Equal(t, want, ClassifyDecline("nmi", shape), "nmi %q (numeric %d)", shape, code)
		}
	}
	for _, code := range nmiRetry {
		_, bucketed := nmi[code]
		require.False(t, bucketed, "nmi %d in two buckets", code)
		require.Equal(t, DeclineRetry, ClassifyNMIResponseCode(code), "nmi %d", code)
		if code != 100 {
			require.Equal(t, CoverageKnownRetry, ClassifyDeclineDetail("nmi", strconv.Itoa(code)).Coverage,
				"published nmi %d must be a decided retry, not an unmapped gap", code)
		}
	}

	pinTable(t, "stripe", map[string]DeclineOutcome{
		"expired_card": fix, "invalid_expiry_month": fix, "invalid_expiry_year": fix, "incorrect_cvc": fix,
		"invalid_cvc": fix, "incorrect_zip": fix, "incorrect_number": fix, "invalid_number": fix,
		"incorrect_pin": fix, "invalid_pin": fix, "pin_try_exceeded": fix, "card_not_supported": fix,
		"currency_not_supported": fix, "do_not_honor": fix, "transaction_not_allowed": fix,
		"call_issuer": fix, "authentication_required": fix,
		"revocation_of_authorization": dead, "revocation_of_all_authorizations": dead,
		"stop_payment_order": dead, "lost_card": dead, "stolen_card": dead, "pickup_card": dead,
		"fraudulent": dead, "invalid_account": dead, "no_account": dead,
	}, stripeDeclineOutcomes, func(s string) string { return s })
	// Money-shaped declines must keep dunning; a bucket-2 row here would stop
	// charging every customer who was merely short this month.
	for _, code := range []string{
		"insufficient_funds", "generic_decline", "card_declined", "processing_error", "issuer_not_available",
		"try_again_later", "duplicate_transaction", "card_velocity_exceeded", "withdrawal_count_limit_exceeded",
	} {
		c := ClassifyDeclineDetail("stripe", code)
		require.Equal(t, DeclineRetry, c.Outcome, "stripe %q", code)
		require.Equal(t, CoverageKnownRetry, c.Coverage, "stripe %q", code)
	}

	// CCBill deliberately has no bucket-3 row: none is live-verified enough to
	// justify terminally cancelling a paying customer (BE-112 stays retry).
	pinTable(t, "ccbill", map[string]DeclineOutcome{
		"be102": fix, "be103": fix, "be107": fix, "be114": fix, "be116": fix, "be132": fix, "be146": fix,
	}, ccbillDeclineOutcomes, func(s string) string { return s })
	require.Equal(t, DeclineRetry, ClassifyDecline("ccbill", "BE-112"))
}

func pinTable[K comparable](t *testing.T, rail string, want, got map[K]DeclineOutcome, wire func(K) string) {
	t.Helper()
	require.Len(t, got, len(want), "%s table size changed; a new bucket row is a policy change", rail)
	for code, outcome := range want {
		require.Contains(t, got, code, "%s %v removed", rail, code)
		require.Equal(t, outcome, got[code], "%s %v moved bucket", rail, code)
		require.Equal(t, outcome, ClassifyDecline(rail, wire(code)), "%s %v via ClassifyDecline", rail, code)
	}
}

// Unbucketed codes are always retry (missing evidence never cancels), and
// coverage separates decided retries from gaps worth alerting on.
func TestClassifyDeclineDetail(t *testing.T) {
	fix, dead := DeclineFixPaymentMethod, DeclineNonRecoverable
	cases := []struct {
		rail, code string
		outcome    DeclineOutcome
		coverage   DeclineCoverage
	}{
		{"nmi", "223", fix, CoverageBucketed},
		{" NMI ", " 262 ", dead, CoverageBucketed},
		{"nmi", "nmi_expired_card", fix, CoverageBucketed},
		{"nmi", "pick_up_card", fix, CoverageBucketed},
		{"stripe", "STOLEN_CARD", dead, CoverageBucketed},
		{"ccbill", "BE-114", fix, CoverageBucketed},
		{"ccbill", " be114 ", fix, CoverageBucketed},
		{"ccbill", "Be-114", fix, CoverageBucketed},

		{"nmi", "202", DeclineRetry, CoverageKnownRetry},
		{"nmi", "insufficient_funds", DeclineRetry, CoverageKnownRetry},
		{"ccbill", "BE-113", DeclineRetry, CoverageKnownRetry},
		{"ccbill", "BE-950", DeclineRetry, CoverageKnownRetry},

		{"nmi", "999", DeclineRetry, CoverageUnrecognized},
		{"nmi", "brand_new_localization_id", DeclineRetry, CoverageUnrecognized},
		{"stripe", "some_code_stripe_added_last_tuesday", DeclineRetry, CoverageUnrecognized},
		{"ccbill", "BE-777", DeclineRetry, CoverageUnrecognized},
		{"ccbill", "stolen_card", DeclineRetry, CoverageUnrecognized},
		{"ccbill", "261", DeclineRetry, CoverageUnrecognized},

		{"nmi", "", DeclineRetry, CoverageNoVocabulary},
		{"stripe", "  ", DeclineRetry, CoverageNoVocabulary},
		{"solana", "declined_stop_all_recurring_payments", DeclineRetry, CoverageNoVocabulary},
		{"vaulted_card", "262", DeclineRetry, CoverageNoVocabulary}, // retired value (or#879)
		{"mobius", "252", DeclineRetry, CoverageNoVocabulary},       // a PSP key, not a rail
		{"a_rail_that_does_not_exist", "252", DeclineRetry, CoverageNoVocabulary},
	}
	for _, c := range cases {
		got := ClassifyDeclineDetail(c.rail, c.code)
		require.Equal(t, c.outcome, got.Outcome, "%q %q outcome", c.rail, c.code)
		require.Equal(t, c.coverage, got.Coverage, "%q %q coverage", c.rail, c.code)
		require.Equal(t, c.coverage == CoverageUnrecognized, got.NeedsMapping(), "%q %q", c.rail, c.code)
		require.Equal(t, c.outcome, ClassifyDecline(c.rail, c.code))
	}
	require.Equal(t, DeclineRetry, ClassifyNMIResponseCode(0))

	var zero DeclineOutcome
	require.Equal(t, DeclineRetry, zero, "callers that never set a decline must get bucket 1")
	require.False(t, DeclineRetry.StopsCharging())
	require.True(t, DeclineFixPaymentMethod.StopsCharging())
	require.True(t, DeclineNonRecoverable.StopsCharging())
}

// Offsets, failure budget, per-failure gaps and derived staleness window per
// cadence tier, including the tier boundaries (#359, #839).
func TestRetrySchedule(t *testing.T) {
	weekly := []time.Duration{day, 2 * day}
	monthly := []time.Duration{2 * day, 5 * day, 9 * day, 13 * day}
	cases := []struct {
		cycleHours int
		offsets    []time.Duration
		gaps       []time.Duration
		window     time.Duration
	}{
		{1, nil, nil, 30 * time.Minute},
		{24, nil, nil, 12 * time.Hour},
		{3 * 24, nil, nil, day},
		{95, nil, nil, day},
		{4 * 24, weekly, []time.Duration{day, day}, 3 * day},
		{7 * 24, weekly, []time.Duration{day, day}, 3 * day},
		{27 * 24, weekly, []time.Duration{day, day}, 3 * day},
		{28 * 24, monthly, []time.Duration{2 * day, 3 * day, 4 * day, 4 * day}, 14 * day},
		{30 * 24, monthly, []time.Duration{2 * day, 3 * day, 4 * day, 4 * day}, 14 * day},
		{365 * 24, monthly, []time.Duration{2 * day, 3 * day, 4 * day, 4 * day}, 14 * day},
	}
	for _, tc := range cases {
		require.Equal(t, tc.offsets, must(RetryOffsets(tc.cycleHours)), "%dh offsets", tc.cycleHours)
		require.Equal(t, len(tc.offsets)+1, must(MaxFailures(tc.cycleHours)), "%dh max failures", tc.cycleHours)
		require.Equal(t, tc.window, must(Window(tc.cycleHours)), "%dh window", tc.cycleHours)
		var cum time.Duration
		for i, gap := range tc.gaps {
			require.Equal(t, gap, must(NextRetryIn(tc.cycleHours, i+1)), "%dh failure %d", tc.cycleHours, i+1)
			cum += gap
			require.Equal(t, tc.offsets[i], cum, "%dh: gaps must telescope to the offsets", tc.cycleHours)
		}
		for _, failures := range []int{0, len(tc.offsets) + 1, 99} {
			require.Zero(t, must(NextRetryIn(tc.cycleHours, failures)), "%dh failure %d is terminal", tc.cycleHours, failures)
		}
	}

	for cycleHours := 1; cycleHours <= 400*24; cycleHours++ {
		require.Less(t, must(Window(cycleHours)), time.Duration(cycleHours)*time.Hour,
			"cycle %dh: window must stay inside one cycle", cycleHours)
	}
}

func TestUnknownCycleFailsClosed(t *testing.T) {
	retryable := "insufficient_funds"
	for _, cycleHours := range []int{0, -1} {
		_, err := RetryOffsets(cycleHours)
		var typed *UnknownCycleError
		require.ErrorAs(t, err, &typed)
		require.Equal(t, cycleHours, typed.CycleHours)
		require.ErrorIs(t, err, ErrUnknownCycle)
		_, err = MaxFailures(cycleHours)
		require.ErrorIs(t, err, ErrUnknownCycle)
		_, err = NextRetryIn(cycleHours, 1)
		require.ErrorIs(t, err, ErrUnknownCycle)
		_, err = Window(cycleHours)
		require.ErrorIs(t, err, ErrUnknownCycle)
		_, err = FailureAction(cycleHours, "nmi", &retryable, 0, nil, time.Now())
		require.ErrorIs(t, err, ErrUnknownCycle)
	}
	// Buckets 2 and 3 do not depend on the cycle.
	for code, outcome := range map[string]DeclineOutcome{"stolen_card": DeclineNonRecoverable, "expired_card": DeclineFixPaymentMethod} {
		a, err := FailureAction(0, "nmi", &code, 0, nil, time.Now())
		require.NoError(t, err)
		require.Equal(t, outcome, a.Outcome)
	}
}

// Every Action names exactly one disposition: schedule, terminal, or (bucket 2
// only) a deliberate stop awaiting a new payment method (or#828).
func TestFailureAction(t *testing.T) {
	first := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	monthlyCycle := CycleHoursBetween(first, first.AddDate(0, 1, 0))
	weeklyCycle := CycleHoursBetween(first, first.AddDate(0, 0, 7))
	dailyCycle := CycleHoursBetween(first, first.AddDate(0, 0, 1))
	at := func(d time.Duration) *time.Time { return ptr(first.Add(d)) }

	cases := []struct {
		name     string
		cycle    int
		rail     string
		code     *string
		prior    int
		first    *time.Time
		outcome  DeclineOutcome
		next     *time.Time
		terminal bool
		unmapped bool
	}{
		{"b1 first failure", monthlyCycle, "nmi", ptr("insufficient_funds"), 0, nil, DeclineRetry, at(2 * day), false, false},
		{"b1 anchored to first failure", monthlyCycle, "nmi", ptr("insufficient_funds"), 2, &first, DeclineRetry, at(9 * day), false, false},
		{"b1 exhausted", monthlyCycle, "nmi", ptr("insufficient_funds"), 4, &first, DeclineRetry, nil, true, false},
		{"b1 weekly statement uses weekly offsets", weeklyCycle, "nmi", ptr("202"), 0, &first, DeclineRetry, at(day), false, false},
		{"b1 sub-4-day cycle terminal at once", dailyCycle, "nmi", ptr("202"), 0, &first, DeclineRetry, nil, true, false},
		{"b1 no code", monthlyCycle, "nmi", nil, 0, nil, DeclineRetry, at(2 * day), false, false},
		{"b1 unmapped code keeps schedule and flags gap", monthlyCycle, "stripe", ptr("a_code_no_rail_published"), 0, nil, DeclineRetry, at(2 * day), false, true},
		{"b1 ccbill cannot read nmi vocabulary", monthlyCycle, "ccbill", ptr("declined_stop_all_recurring_payments"), 0, nil, DeclineRetry, at(2 * day), false, true},
		{"b2 expired card", monthlyCycle, "nmi", ptr("expired_card"), 0, nil, DeclineFixPaymentMethod, nil, false, false},
		{"b2 wire form", monthlyCycle, "nmi", ptr("nmi_response_223"), 3, &first, DeclineFixPaymentMethod, nil, false, false},
		{"b2 lost card is fixable", monthlyCycle, "nmi", ptr("pick_up_card"), 0, nil, DeclineFixPaymentMethod, nil, false, false},
		{"b2 stripe", monthlyCycle, "stripe", ptr("expired_card"), 0, nil, DeclineFixPaymentMethod, nil, false, false},
		{"b3 mandate withdrawn", monthlyCycle, "nmi", ptr("declined_stop_all_recurring_payments"), 0, nil, DeclineNonRecoverable, nil, true, false},
		{"b3 wire form", monthlyCycle, "nmi", ptr("nmi_response_261"), 0, nil, DeclineNonRecoverable, nil, true, false},
		{"b3 stripe", monthlyCycle, "stripe", ptr("revocation_of_authorization"), 0, nil, DeclineNonRecoverable, nil, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := must(FailureAction(tc.cycle, tc.rail, tc.code, tc.prior, tc.first, first))
			require.Equal(t, tc.outcome, a.Outcome)
			require.Equal(t, tc.outcome, a.Decline.Outcome)
			require.Equal(t, tc.terminal, a.Terminal)
			require.Equal(t, tc.next, a.NextAttemptAt)
			require.Equal(t, tc.unmapped, a.Decline.NeedsMapping())
			require.Equal(t, tc.outcome == DeclineFixPaymentMethod, a.AwaitingPaymentMethod())
			require.Equal(t, tc.terminal && tc.outcome == DeclineRetry, a.ScheduleExhausted())
			if a.NextAttemptAt == nil && !a.Terminal {
				require.True(t, a.AwaitingPaymentMethod(), "neither retry nor terminal is legal only for bucket 2")
			}
		})
	}

	// Invoice and subscription consumers walk the same offsets for a cycle.
	for _, cycle := range []int{weeklyCycle, MonthlyCycleHours, monthlyCycle, 365 * 24} {
		offsets := must(RetryOffsets(cycle))
		for prior := range offsets {
			a := must(FailureAction(cycle, "nmi", ptr("202"), prior, &first, first.Add(30*day)))
			require.Equal(t, first.Add(offsets[prior]), *a.NextAttemptAt, "cycle %dh prior %d", cycle, prior)
		}
		require.True(t, must(FailureAction(cycle, "nmi", ptr("202"), len(offsets), &first, first)).ScheduleExhausted())
	}
}

func TestCycleHours(t *testing.T) {
	from := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
	require.Equal(t, 168, CycleHoursBetween(from, from.AddDate(0, 0, 7)))
	require.Equal(t, 744, CycleHoursBetween(from, from.AddDate(0, 1, 0)))
	require.Zero(t, CycleHoursBetween(time.Time{}, from), "unset period is unknown")
	require.Zero(t, CycleHoursBetween(from, time.Time{}))
	require.Zero(t, CycleHoursBetween(from, from), "degenerate period is unknown")
	require.Zero(t, CycleHoursBetween(from, from.AddDate(0, 0, -1)), "inverted period is unknown")

	week := 168
	require.Zero(t, BillingCycleHoursOf(nil))
	require.Zero(t, BillingCycleHoursOf(&models.Price{}))
	require.Zero(t, BillingCycleHoursOf(&models.Price{AccessDurationHours: &week}), "one-time price has no cycle")
	require.Equal(t, week, BillingCycleHoursOf(&models.Price{AccessDurationHours: &week, AutoRenew: true}))
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func ptr[T any](v T) *T { return &v }
