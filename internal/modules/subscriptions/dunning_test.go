package subscriptions

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

// TestDunningRetryOffsets pins the cadence-relative tier table (#359),
// including the exact tier boundaries. The binding principle behind the
// boundaries: the derived staleness window (last retry offset + one day of
// slack) must fit inside ONE billing cycle, so a sub is never still dunning
// the old period once the next one is due.
func TestDunningRetryOffsets(t *testing.T) {
	day := 24 * time.Hour
	weekly := []time.Duration{1 * day, 2 * day}
	monthly := []time.Duration{2 * day, 5 * day, 9 * day, 13 * day}

	cases := []struct {
		name        string
		cycleDays   int
		offsets     []time.Duration
		maxFailures int
		window      time.Duration
	}{
		// Anchors.
		{"daily: no dunning, first failure terminal", 1, nil, 1, 0},
		{"weekly: retries at +1d, +2d", 7, weekly, 3, 3 * day},
		{"monthly: progressive +2d/+5d/+9d/+13d", 30, monthly, 5, 14 * day},
		{"yearly: capped at the monthly schedule", 365, monthly, 5, 14 * day},

		// Boundaries of the 0-retry tier: a 2-3 day cycle retried daily would
		// still be dunning when the next period is due (window 3d > cycle).
		{"2d: still no dunning", 2, nil, 1, 0},
		{"3d: still no dunning", 3, nil, 1, 0},
		{"4d: first cycle with retries (window 3d fits inside the cycle)", 4, weekly, 3, 3 * day},

		// Boundaries of the monthly tier: the 14d window must fit well inside
		// the cycle; 28d covers 4-weekly "monthly" openrails.
		{"27d: weekly tier (the monthly 14d window would not fit well)", 27, weekly, 3, 3 * day},
		{"28d: monthly tier starts (4-weekly billing)", 28, monthly, 5, 14 * day},

		// Unknown cycle (one-time price): defensive monthly fallback.
		{"unknown (0): monthly fallback", 0, monthly, 5, 14 * day},
		{"unknown (negative): monthly fallback", -1, monthly, 5, 14 * day},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.offsets, DunningRetryOffsets(tc.cycleDays), "offsets")
			require.Equal(t, tc.maxFailures, DunningMaxFailures(tc.cycleDays), "maxFailures")
			require.Equal(t, tc.window, DunningWindow(tc.cycleDays), "derived window")
		})
	}
}

// TestDunningNextRetryIn pins the per-failure gaps: when each retry runs on
// time, the gaps reproduce the offset schedule exactly (monthly example: fail
// June 1 -> June 3, 6, 10, 14 -> terminal).
func TestDunningNextRetryIn(t *testing.T) {
	day := 24 * time.Hour

	// Monthly gaps: 2d, 3d, 4d, 4d; the 5th failure is terminal.
	require.Equal(t, 2*day, DunningNextRetryIn(30, 1))
	require.Equal(t, 3*day, DunningNextRetryIn(30, 2))
	require.Equal(t, 4*day, DunningNextRetryIn(30, 3))
	require.Equal(t, 4*day, DunningNextRetryIn(30, 4))
	require.Equal(t, time.Duration(0), DunningNextRetryIn(30, 5), "5th monthly failure is terminal")

	// Weekly gaps: 1d, 1d; the 3rd failure is terminal.
	require.Equal(t, 1*day, DunningNextRetryIn(7, 1))
	require.Equal(t, 1*day, DunningNextRetryIn(7, 2))
	require.Equal(t, time.Duration(0), DunningNextRetryIn(7, 3), "3rd weekly failure is terminal")

	// Daily: the first failure is terminal.
	require.Equal(t, time.Duration(0), DunningNextRetryIn(1, 1))

	// Out-of-range failure counts never schedule a retry.
	require.Equal(t, time.Duration(0), DunningNextRetryIn(30, 0))
	require.Equal(t, time.Duration(0), DunningNextRetryIn(30, 99))
}

// TestDunningWindowFitsInsideOneCycle asserts the boundary principle itself
// across every cycle length that gets retries: the derived window never
// reaches the next rebill.
func TestDunningWindowFitsInsideOneCycle(t *testing.T) {
	day := 24 * time.Hour
	for cycleDays := dunningMinRetryCycleDays; cycleDays <= 400; cycleDays++ {
		window := DunningWindow(cycleDays)
		cycle := time.Duration(cycleDays) * day
		require.Less(t, window, cycle,
			"cycle %dd: derived window %s must stay inside one billing cycle", cycleDays, window)
	}
}

func TestBillingCycleDaysOf(t *testing.T) {
	require.Equal(t, 0, BillingCycleDaysOf(nil))
	require.Equal(t, 0, BillingCycleDaysOf(&models.Price{}))
	seven := 7
	require.Equal(t, 7, BillingCycleDaysOf(&models.Price{BillingCycleDays: &seven}))
}

// TestGraceSlack pins the renewal grace window derivation (#368): half the
// billing cycle, capped at 48h; unknown cycles defensively get the cap.
func TestGraceSlack(t *testing.T) {
	cases := []struct {
		cycleDays int
		want      time.Duration
	}{
		{0, 48 * time.Hour},   // unknown -> monthly cap, defensively
		{-3, 48 * time.Hour},  // nonsense -> monthly cap, defensively
		{1, 12 * time.Hour},   // daily: half a day
		{2, 24 * time.Hour},   // 2-day: one day
		{5, 48 * time.Hour},   // capped (half = 60h)
		{6, 48 * time.Hour},   // capped (half = 72h)
		{7, 48 * time.Hour},   // weekly: capped (half = 84h)
		{30, 48 * time.Hour},  // monthly: capped
		{365, 48 * time.Hour}, // yearly: never more generous than monthly
	}
	for _, c := range cases {
		if got := GraceSlack(c.cycleDays); got != c.want {
			t.Errorf("GraceSlack(%d) = %s, want %s", c.cycleDays, got, c.want)
		}
	}
}

// TestRenewalGraceEligibleProcessor pins which processors get the
// pre-appended renewal grace window: NMI-backed + Stripe only.
func TestRenewalGraceEligibleProcessor(t *testing.T) {
	if !RenewalGraceEligibleProcessor(models.ProcessorMobius) {
		t.Error("mobius (NMI-backed) must be grace-eligible")
	}
	if !RenewalGraceEligibleProcessor(models.ProcessorStripe) {
		t.Error("stripe must be grace-eligible")
	}
	for _, p := range []models.Processor{models.ProcessorCCBill, models.ProcessorSolana, models.ProcessorPayPal, models.ProcessorAdmin, models.ProcessorManual} {
		if RenewalGraceEligibleProcessor(p) {
			t.Errorf("%s must NOT be grace-eligible", p)
		}
	}
}
