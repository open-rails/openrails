package payments

import (
	"testing"
	"time"
)

func TestARefundReportedAWeekLateIsNoNews(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		ago  time.Duration
		want bool
	}{
		{0, true}, {10 * time.Minute, true}, {RefundReceiptWindow, true},
		{RefundReceiptWindow + time.Second, false}, {8 * 24 * time.Hour, false},
		{-time.Minute, true}, // a provider clock ahead of ours
	} {
		if got := recent(now.Add(-c.ago), now); got != c.want {
			t.Errorf("refunded %s ago: %v", c.ago, got)
		}
	}
}
