package collection

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/decline"
)

const day = 24 * time.Hour

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
	for code, outcome := range map[string]decline.Action{"stolen_card": decline.NonRecoverable, "expired_card": decline.FixPaymentMethod} {
		a, err := FailureAction(0, "nmi", &code, 0, nil, time.Now())
		require.NoError(t, err)
		require.Equal(t, outcome, a.Decline.Action)
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
		outcome  decline.Action
		next     *time.Time
		terminal bool
		unmapped bool
	}{
		{"b1 first failure", monthlyCycle, "nmi", ptr("insufficient_funds"), 0, nil, decline.Retry, at(2 * day), false, false},
		{"b1 anchored to first failure", monthlyCycle, "nmi", ptr("insufficient_funds"), 2, &first, decline.Retry, at(9 * day), false, false},
		{"b1 exhausted", monthlyCycle, "nmi", ptr("insufficient_funds"), 4, &first, decline.Retry, nil, true, false},
		{"b1 weekly statement uses weekly offsets", weeklyCycle, "nmi", ptr("202"), 0, &first, decline.Retry, at(day), false, false},
		{"b1 sub-4-day cycle terminal at once", dailyCycle, "nmi", ptr("202"), 0, &first, decline.Retry, nil, true, false},
		{"b1 no code", monthlyCycle, "nmi", nil, 0, nil, decline.Retry, at(2 * day), false, false},
		{"b1 unmapped code keeps schedule and flags gap", monthlyCycle, "stripe", ptr("a_code_no_rail_published"), 0, nil, decline.Retry, at(2 * day), false, true},
		{"b1 ccbill cannot read nmi vocabulary", monthlyCycle, "ccbill", ptr("declined_stop_all_recurring_payments"), 0, nil, decline.Retry, at(2 * day), false, true},
		{"b2 expired card", monthlyCycle, "nmi", ptr("expired_card"), 0, nil, decline.FixPaymentMethod, nil, false, false},
		{"b2 wire form", monthlyCycle, "nmi", ptr("nmi_response_223"), 3, &first, decline.FixPaymentMethod, nil, false, false},
		{"b2 lost card is fixable", monthlyCycle, "nmi", ptr("pick_up_card"), 0, nil, decline.FixPaymentMethod, nil, false, false},
		{"b2 stripe", monthlyCycle, "stripe", ptr("expired_card"), 0, nil, decline.FixPaymentMethod, nil, false, false},
		{"b3 mandate withdrawn", monthlyCycle, "nmi", ptr("declined_stop_all_recurring_payments"), 0, nil, decline.NonRecoverable, nil, true, false},
		{"b3 wire form", monthlyCycle, "nmi", ptr("nmi_response_261"), 0, nil, decline.NonRecoverable, nil, true, false},
		{"b3 stripe", monthlyCycle, "stripe", ptr("revocation_of_authorization"), 0, nil, decline.NonRecoverable, nil, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := must(FailureAction(tc.cycle, tc.rail, tc.code, tc.prior, tc.first, first))
			require.Equal(t, tc.outcome, a.Decline.Action)
			require.Equal(t, tc.terminal, a.Terminal)
			require.Equal(t, tc.next, a.NextAttemptAt)
			require.Equal(t, tc.unmapped, a.Decline.NeedsMapping())
			require.Equal(t, tc.outcome == decline.FixPaymentMethod, a.AwaitingPaymentMethod())
			require.Equal(t, tc.terminal && tc.outcome == decline.Retry, a.ScheduleExhausted())
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
	require.Equal(t, week, BillingCycleHoursOf(&models.Price{AccessDurationHours: &week, BillingIntervalHours: &week}))
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func ptr[T any](v T) *T { return &v }

func TestTransientDeclines(t *testing.T) {
	t.Parallel()
	require.True(t, decline.Classify("stripe", "processing_error").Transient)
	require.True(t, decline.Classify("Stripe", "try_again_later").Transient)
	require.False(t, decline.Classify("stripe", "insufficient_funds").Transient)
	require.False(t, decline.Classify("stripe", "expired_card").Transient)
	require.False(t, decline.Classify("nmi", "202").Transient)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	next, ok := NextTransientAttempt(0, at)
	require.True(t, ok)
	require.Equal(t, at.Add(5*time.Minute), next)
	next, ok = NextTransientAttempt(1, at)
	require.True(t, ok)
	require.Equal(t, at.Add(30*time.Minute), next)
	_, ok = NextTransientAttempt(2, at)
	require.False(t, ok, "the ladder is bounded")
}
