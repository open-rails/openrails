package collection

import (
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

func TestClassifyNMIDecline(t *testing.T) {
	hard := []int{201, 204, 220, 221, 222, 223, 224, 225, 226, 240, 250, 251, 252, 253, 261, 262, 263, 461}
	for _, code := range hard {
		if got := ClassifyNMIDecline(code); got != DeclineHard {
			t.Errorf("code %d: expected DeclineHard, got %v", code, got)
		}
	}

	soft := []int{0, 100, 200, 202, 203, 260, 264, 300, 400, 410, 411, 420, 421, 430, 440, 441, 460, 999}
	for _, code := range soft {
		if got := ClassifyNMIDecline(code); got != DeclineSoft {
			t.Errorf("code %d: expected DeclineSoft, got %v", code, got)
		}
	}
}

// TestRetryOffsets pins the cadence-relative tier table (#359), including the
// exact tier boundaries. The binding principle: the derived staleness window
// (last retry offset + one day of slack) must fit inside ONE billing cycle.
//
// #839: the 0-retry tier's window is the SLACK, never zero. A zero window is
// true by construction the instant a row lapses, so it gave a short-cycle
// subscription no chance to be charged at all before collection wrote it off.
func TestRetryOffsets(t *testing.T) {
	day := 24 * time.Hour
	weekly := []time.Duration{1 * day, 2 * day}
	monthly := []time.Duration{2 * day, 5 * day, 9 * day, 13 * day}

	cases := []struct {
		name        string
		cycleHours  int
		offsets     []time.Duration
		maxFailures int
		window      time.Duration
	}{
		// Anchors.
		{"daily: no retries, but a full day of slack to attempt the charge", 24, nil, 1, 1 * day},
		{"weekly: retries at +1d, +2d", 7 * 24, weekly, 3, 3 * day},
		{"monthly: progressive +2d/+5d/+9d/+13d", 30 * 24, monthly, 5, 14 * day},
		{"yearly: capped at the monthly schedule", 365 * 24, monthly, 5, 14 * day},

		// Boundaries of the 0-retry tier: a 2-3 day cycle retried daily would
		// still be dunning when the next period is due (window 3d > cycle).
		{"2d: still no retries", 2 * 24, nil, 1, 1 * day},
		{"3d: still no retries", 3 * 24, nil, 1, 1 * day},
		{"4d: first cycle with retries (window 3d fits inside the cycle)", 4 * 24, weekly, 3, 3 * day},

		// Boundaries of the monthly tier: the 14d window must fit well inside
		// the cycle; 28d covers 4-weekly "monthly" billing.
		{"27d: weekly tier (the monthly 14d window would not fit well)", 27 * 24, weekly, 3, 3 * day},
		{"28d: monthly tier starts (4-weekly billing)", 28 * 24, monthly, 5, 14 * day},

		// Unknown cycle (one-time price): defensive monthly fallback.
		{"unknown (0): monthly fallback", 0, monthly, 5, 14 * day},
		{"unknown (negative): monthly fallback", -1, monthly, 5, 14 * day},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.offsets, RetryOffsets(tc.cycleHours), "offsets")
			require.Equal(t, tc.maxFailures, MaxFailures(tc.cycleHours), "maxFailures")
			require.Equal(t, tc.window, Window(tc.cycleHours), "derived window")
		})
	}
}

// TestNextRetryIn pins the per-failure gaps: when each retry runs on time,
// the gaps reproduce the offset schedule exactly (monthly example: fail
// June 1 -> June 3, 6, 10, 14 -> terminal).
func TestNextRetryIn(t *testing.T) {
	day := 24 * time.Hour

	// Monthly gaps: 2d, 3d, 4d, 4d; the 5th failure is terminal.
	require.Equal(t, 2*day, NextRetryIn(30*24, 1))
	require.Equal(t, 3*day, NextRetryIn(30*24, 2))
	require.Equal(t, 4*day, NextRetryIn(30*24, 3))
	require.Equal(t, 4*day, NextRetryIn(30*24, 4))
	require.Equal(t, time.Duration(0), NextRetryIn(30*24, 5), "5th monthly failure is terminal")

	// Weekly gaps: 1d, 1d; the 3rd failure is terminal.
	require.Equal(t, 1*day, NextRetryIn(7*24, 1))
	require.Equal(t, 1*day, NextRetryIn(7*24, 2))
	require.Equal(t, time.Duration(0), NextRetryIn(7*24, 3), "3rd weekly failure is terminal")

	// Daily: the first failure is terminal.
	require.Equal(t, time.Duration(0), NextRetryIn(24, 1))

	// Out-of-range failure counts never schedule a retry.
	require.Equal(t, time.Duration(0), NextRetryIn(30*24, 0))
	require.Equal(t, time.Duration(0), NextRetryIn(30*24, 99))
}

// TestWindowFitsInsideOneCycle asserts the boundary principle itself across
// every cycle length that gets retries.
func TestWindowFitsInsideOneCycle(t *testing.T) {
	for cycleHours := MinRetryCycleHours; cycleHours <= 400*24; cycleHours += 24 {
		window := Window(cycleHours)
		cycle := time.Duration(cycleHours) * time.Hour
		require.Less(t, window, cycle,
			"cycle %dh: derived window %s must stay inside one billing cycle", cycleHours, window)
	}
}

func TestBillingCycleHoursOf(t *testing.T) {
	require.Equal(t, 0, BillingCycleHoursOf(nil))
	require.Equal(t, 0, BillingCycleHoursOf(&models.Price{}))
	seven := 168
	require.Equal(t, 168, BillingCycleHoursOf(&models.Price{AccessDurationHours: &seven, AutoRenew: true}))
}

// TestFailureAction pins the invoice-consumer failure mechanics on the SAME
// offsets table (moved from money/invoice_dunning.go by #828).
func TestFailureAction(t *testing.T) {
	t.Parallel()

	first := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	insufficientFunds := "insufficient_funds"
	expiredCard := "expired_card"

	tests := []struct {
		name            string
		code            *string
		currentFailures int
		firstFailureAt  *time.Time
		wantNext        *time.Time
		wantTerminal    bool
	}{
		{
			name:     "first recoverable failure retries on day two",
			code:     &insufficientFunds,
			wantNext: timePointer(first.Add(2 * 24 * time.Hour)),
		},
		{
			name:            "later retry remains anchored to first failure",
			code:            &insufficientFunds,
			currentFailures: 2,
			firstFailureAt:  &first,
			wantNext:        timePointer(first.Add(9 * 24 * time.Hour)),
		},
		{
			name:            "fifth recoverable failure is terminal",
			code:            &insufficientFunds,
			currentFailures: 4,
			firstFailureAt:  &first,
			wantTerminal:    true,
		},
		{
			name: "card replacement failure pauses retries",
			code: &expiredCard,
		},
		{
			name: "unknown failure pauses retries",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			action := FailureAction(MonthlyCycleHours, "nmi", test.code, test.currentFailures, test.firstFailureAt, first)
			require.Equal(t, test.wantTerminal, action.Terminal)
			if test.wantNext == nil {
				require.Nil(t, action.NextAttemptAt)
			} else {
				require.NotNil(t, action.NextAttemptAt)
				require.True(t, action.NextAttemptAt.Equal(*test.wantNext))
			}
		})
	}
}

// TestScheduleParity is the #828 unification proof at the table level: the
// invoice consumer's failure actions walk EXACTLY the subscription consumer's
// retry offsets for the same billing cycle — one schedule, two consumers.
func TestScheduleParity(t *testing.T) {
	code := "insufficient_funds"
	for _, cycleHours := range []int{MonthlyCycleHours, 30 * 24, 7 * 24, 365 * 24} {
		offsets := RetryOffsets(cycleHours)
		first := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
		for failures := 0; failures < len(offsets); failures++ {
			action := FailureAction(cycleHours, "nmi", &code, failures, &first, first)
			require.NotNil(t, action.NextAttemptAt, "cycle %dh failure %d", cycleHours, failures)
			require.Equal(t, first.Add(offsets[failures]), *action.NextAttemptAt,
				"cycle %dh: invoice next-attempt must equal subscription offset %d", cycleHours, failures)
			// The subscription formulation (gaps) telescopes to the same offsets.
			var cum time.Duration
			for f := 1; f <= failures+1; f++ {
				cum += NextRetryIn(cycleHours, f)
			}
			require.Equal(t, offsets[failures], cum,
				"cycle %dh: cumulative NextRetryIn gaps must telescope to offset %d", cycleHours, failures)
		}
		action := FailureAction(cycleHours, "nmi", &code, len(offsets), &first, first)
		require.True(t, action.Terminal, "cycle %dh: exhausting the offsets is terminal on both paths", cycleHours)
		require.Equal(t, len(offsets)+1, MaxFailures(cycleHours))
	}
}

func timePointer(value time.Time) *time.Time { return &value }
