package checkout

import (
	"testing"
	"time"
)

func TestCalculateProrationRoundsUp(t *testing.T) {
	svc := &CheckoutService{}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cycleDays := 30

	cases := []struct {
		name       string
		periodEnd  time.Time
		oldPrice   int64
		newPrice   int64
		wantDays   int
		wantAmount int64
	}{
		{
			// 1 hour left must still be charged as a full day (was $0 before fix).
			name:       "one hour left charges one day",
			periodEnd:  now.Add(1 * time.Hour),
			oldPrice:   1000,
			newPrice:   4000, // diff 3000 over 30 days => 100/day
			wantDays:   1,
			wantAmount: 100,
		},
		{
			// 25 hours = just over a day rounds up to 2 days.
			name:       "just over one day rounds up to two",
			periodEnd:  now.Add(25 * time.Hour),
			oldPrice:   1000,
			newPrice:   4000,
			wantDays:   2,
			wantAmount: 200,
		},
		{
			// Exactly 10 whole days stays 10 days.
			name:       "exact whole days unchanged",
			periodEnd:  now.Add(10 * 24 * time.Hour),
			oldPrice:   1000,
			newPrice:   4000,
			wantDays:   10,
			wantAmount: 1000,
		},
		{
			// Period already ended: nothing remaining.
			name:       "period ended charges nothing",
			periodEnd:  now.Add(-time.Hour),
			oldPrice:   1000,
			newPrice:   4000,
			wantDays:   0,
			wantAmount: 0,
		},
		{
			// Downgrade never charges proration.
			name:       "downgrade charges nothing",
			periodEnd:  now.Add(5 * 24 * time.Hour),
			oldPrice:   4000,
			newPrice:   1000,
			wantDays:   5,
			wantAmount: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			periodEnd := tc.periodEnd
			amount, days, gotCycle := svc.CalculateProration(tc.oldPrice, tc.newPrice, &periodEnd, &cycleDays, now)
			if days != tc.wantDays {
				t.Fatalf("daysRemaining: got %d, want %d", days, tc.wantDays)
			}
			if amount != tc.wantAmount {
				t.Fatalf("prorationAmount: got %d, want %d", amount, tc.wantAmount)
			}
			if gotCycle != cycleDays {
				t.Fatalf("cycleDays: got %d, want %d", gotCycle, cycleDays)
			}
		})
	}
}
