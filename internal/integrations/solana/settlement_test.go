package solana

import (
	"testing"
	"time"
)

// SEC-33: a quote is honoured only within its late window.
func TestSettlementTooLate(t *testing.T) {
	expires := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := expires.Add(d); return &v }
	for _, tc := range []struct {
		landed *time.Time
		late   bool
	}{
		{at(-time.Minute), false},
		{at(LateSettlementWindow), false},
		{at(LateSettlementWindow + time.Second), true},
		{at(48 * time.Hour), true},
		{nil, false},
	} {
		if got := SettlementTooLate(tc.landed, expires); got != tc.late {
			t.Fatalf("landed %v: late=%v, want %v", tc.landed, got, tc.late)
		}
	}
	if SettlementTooLate(at(time.Hour), time.Time{}) {
		t.Fatal("a quote without expiry is never late")
	}
}
