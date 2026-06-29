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
		cycleHours  int
		offsets     []time.Duration
		maxFailures int
		window      time.Duration
	}{
		// Anchors.
		{"daily: no dunning, first failure terminal", 24, nil, 1, 0},
		{"weekly: retries at +1d, +2d", 7 * 24, weekly, 3, 3 * day},
		{"monthly: progressive +2d/+5d/+9d/+13d", 30 * 24, monthly, 5, 14 * day},
		{"yearly: capped at the monthly schedule", 365 * 24, monthly, 5, 14 * day},

		// Boundaries of the 0-retry tier: a 2-3 day cycle retried daily would
		// still be dunning when the next period is due (window 3d > cycle).
		{"2d: still no dunning", 2 * 24, nil, 1, 0},
		{"3d: still no dunning", 3 * 24, nil, 1, 0},
		{"4d: first cycle with retries (window 3d fits inside the cycle)", 4 * 24, weekly, 3, 3 * day},

		// Boundaries of the monthly tier: the 14d window must fit well inside
		// the cycle; 28d covers 4-weekly "monthly" openrails.
		{"27d: weekly tier (the monthly 14d window would not fit well)", 27 * 24, weekly, 3, 3 * day},
		{"28d: monthly tier starts (4-weekly billing)", 28 * 24, monthly, 5, 14 * day},

		// Unknown cycle (one-time price): defensive monthly fallback.
		{"unknown (0): monthly fallback", 0, monthly, 5, 14 * day},
		{"unknown (negative): monthly fallback", -1, monthly, 5, 14 * day},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.offsets, DunningRetryOffsets(tc.cycleHours), "offsets")
			require.Equal(t, tc.maxFailures, DunningMaxFailures(tc.cycleHours), "maxFailures")
			require.Equal(t, tc.window, DunningWindow(tc.cycleHours), "derived window")
		})
	}
}

// TestDunningNextRetryIn pins the per-failure gaps: when each retry runs on
// time, the gaps reproduce the offset schedule exactly (monthly example: fail
// June 1 -> June 3, 6, 10, 14 -> terminal).
func TestDunningNextRetryIn(t *testing.T) {
	day := 24 * time.Hour

	// Monthly gaps: 2d, 3d, 4d, 4d; the 5th failure is terminal.
	require.Equal(t, 2*day, DunningNextRetryIn(30*24, 1))
	require.Equal(t, 3*day, DunningNextRetryIn(30*24, 2))
	require.Equal(t, 4*day, DunningNextRetryIn(30*24, 3))
	require.Equal(t, 4*day, DunningNextRetryIn(30*24, 4))
	require.Equal(t, time.Duration(0), DunningNextRetryIn(30*24, 5), "5th monthly failure is terminal")

	// Weekly gaps: 1d, 1d; the 3rd failure is terminal.
	require.Equal(t, 1*day, DunningNextRetryIn(7*24, 1))
	require.Equal(t, 1*day, DunningNextRetryIn(7*24, 2))
	require.Equal(t, time.Duration(0), DunningNextRetryIn(7*24, 3), "3rd weekly failure is terminal")

	// Daily: the first failure is terminal.
	require.Equal(t, time.Duration(0), DunningNextRetryIn(24, 1))

	// Out-of-range failure counts never schedule a retry.
	require.Equal(t, time.Duration(0), DunningNextRetryIn(30*24, 0))
	require.Equal(t, time.Duration(0), DunningNextRetryIn(30*24, 99))
}

// TestDunningWindowFitsInsideOneCycle asserts the boundary principle itself
// across every cycle length that gets retries: the derived window never
// reaches the next rebill.
func TestDunningWindowFitsInsideOneCycle(t *testing.T) {
	for cycleHours := dunningMinRetryCycleHours; cycleHours <= 400*24; cycleHours += 24 {
		window := DunningWindow(cycleHours)
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

// TestGraceSlack pins the renewal grace window derivation (#368): half the
// billing cycle in hours, capped at 48h; unknown cycles defensively get the cap.
func TestGraceSlack(t *testing.T) {
	cases := []struct {
		cycleHours int
		want       time.Duration
	}{
		{0, 48 * time.Hour},        // unknown -> monthly cap, defensively
		{-3, 48 * time.Hour},       // nonsense -> monthly cap, defensively
		{24, 12 * time.Hour},       // daily: half a day
		{2 * 24, 24 * time.Hour},   // 2-day: one day
		{5 * 24, 48 * time.Hour},   // capped (half = 60h)
		{6 * 24, 48 * time.Hour},   // capped (half = 72h)
		{7 * 24, 48 * time.Hour},   // weekly: capped (half = 84h)
		{30 * 24, 48 * time.Hour},  // monthly: capped
		{365 * 24, 48 * time.Hour}, // yearly: never more generous than monthly
	}
	for _, c := range cases {
		if got := GraceSlack(c.cycleHours); got != c.want {
			t.Errorf("GraceSlack(%d) = %s, want %s", c.cycleHours, got, c.want)
		}
	}
}

// TestRenewalGraceEligibleRail pins which rails get the
// pre-appended renewal grace window: NMI-backed + Stripe only.
func TestRenewalGraceEligibleRail(t *testing.T) {
	if !RenewalGraceEligibleRail(models.RailMobius) {
		t.Error("mobius (NMI-backed) must be grace-eligible")
	}
	if !RenewalGraceEligibleRail(models.RailStripe) {
		t.Error("stripe must be grace-eligible")
	}
	for _, p := range []models.Rail{models.RailCCBill, models.RailSolana, models.RailPayPal, models.RailAdmin, models.RailManual} {
		if RenewalGraceEligibleRail(p) {
			t.Errorf("%s must NOT be grace-eligible", p)
		}
	}
}
