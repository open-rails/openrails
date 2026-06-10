package credits

import (
	"testing"
	"time"
)

func p64(v int64) *int64 { return &v }

func TestEvaluateSpend_NoCaps(t *testing.T) {
	dec := evaluateSpend(nil, 500, true, 80, time.Now())
	if !dec.Allowed || dec.DenyCode != "" {
		t.Fatalf("expected allowed with no caps, got %+v", dec)
	}
}

func TestEvaluateSpend_UnderCap(t *testing.T) {
	reset := time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC)
	caps := []capInput{{DenyDailyCap, p64(1000), 400, &reset}}
	dec := evaluateSpend(caps, 500, true, 80, time.Now())
	if !dec.Allowed {
		t.Fatalf("400+500 <= 1000 should be allowed, got %+v", dec)
	}
	if len(dec.Caps) != 1 || dec.Caps[0].Remaining != 600 {
		t.Fatalf("remaining should be 600, got %+v", dec.Caps)
	}
}

func TestEvaluateSpend_OverDailyCap(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	reset := time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC)
	caps := []capInput{{DenyDailyCap, p64(1000), 800, &reset}}
	dec := evaluateSpend(caps, 500, true, 80, now)
	if dec.Allowed {
		t.Fatalf("800+500 > 1000 should be denied")
	}
	if dec.DenyCode != DenyDailyCap {
		t.Fatalf("expected %s, got %s", DenyDailyCap, dec.DenyCode)
	}
	wantRetry := int64(reset.Sub(now).Seconds())
	if dec.RetryAfterSeconds != wantRetry {
		t.Fatalf("retry_after %d != %d", dec.RetryAfterSeconds, wantRetry)
	}
	if dec.NextAllowedAt == nil || !dec.NextAllowedAt.Equal(reset) {
		t.Fatalf("next_allowed_at should be reset")
	}
}

func TestEvaluateSpend_ActorCapBindsFirst(t *testing.T) {
	now := time.Now()
	reset := now.Add(time.Hour)
	// Actor cap listed first and is the binding one even though the org cap is also over.
	caps := []capInput{
		{DenyActorDailyCap, p64(100), 100, &reset},
		{DenyDailyCap, p64(100000), 0, &reset},
	}
	dec := evaluateSpend(caps, 50, true, 80, now)
	if dec.Allowed || dec.DenyCode != DenyActorDailyCap {
		t.Fatalf("expected actor deny first, got %+v", dec)
	}
}

func TestEvaluateSpend_WarnOnlyWhenNotHardStop(t *testing.T) {
	now := time.Now()
	reset := now.Add(time.Hour)
	caps := []capInput{{DenyDailyCap, p64(100), 100, &reset}}
	dec := evaluateSpend(caps, 50, false /* hardStop */, 80, now)
	if !dec.Allowed {
		t.Fatalf("warn-only must allow")
	}
	if dec.DenyCode != DenyDailyCap {
		t.Fatalf("warn-only must still report the breached cap, got %q", dec.DenyCode)
	}
}

func TestEvaluateSpend_AlertThreshold(t *testing.T) {
	now := time.Now()
	reset := now.Add(time.Hour)
	// 85% utilization after the estimate (850/1000) should trip an 80% alert but still allow.
	caps := []capInput{{DenyDailyCap, p64(1000), 800, &reset}}
	dec := evaluateSpend(caps, 50, true, 80, now)
	if !dec.Allowed {
		t.Fatalf("850 <= 1000 should allow")
	}
	if dec.AlertCode != DenyDailyCap {
		t.Fatalf("expected alert at 85%%, got %q (caps=%+v)", dec.AlertCode, dec.Caps)
	}
	if dec.Caps[0].UtilizationPct != 85 {
		t.Fatalf("utilization should be 85, got %d", dec.Caps[0].UtilizationPct)
	}
}

func TestEvaluateSpend_ZeroCapDeniesAnySpend(t *testing.T) {
	dec := evaluateSpend([]capInput{{DenyDailyCap, p64(0), 0, nil}}, 1, true, 80, time.Now())
	if dec.Allowed {
		t.Fatalf("a zero cap must deny any positive estimate")
	}
}

func TestEvaluateSpend_OutstandingNoReset(t *testing.T) {
	dec := evaluateSpend([]capInput{{DenyOutstandingCap, p64(50000), 49000, nil}}, 2000, true, 80, time.Now())
	if dec.Allowed {
		t.Fatalf("49000+2000 > 50000 should deny outstanding")
	}
	if dec.DenyCode != DenyOutstandingCap || dec.RetryAfterSeconds != 0 {
		t.Fatalf("outstanding deny should have no retry window, got %+v", dec)
	}
}

func TestEvaluateSpend_NegativeEstimateTreatedZero(t *testing.T) {
	dec := evaluateSpend([]capInput{{DenyDailyCap, p64(100), 100, nil}}, -50, true, 80, time.Now())
	if !dec.Allowed {
		t.Fatalf("100+max(0,-50)=100 <= 100 should allow, got %+v", dec)
	}
}

func TestDayMonthWindow(t *testing.T) {
	now := time.Date(2026, 6, 2, 15, 30, 0, 0, time.UTC)
	ds, dr := dayWindow(now)
	if ds != time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC) || dr != time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("day window wrong: %v..%v", ds, dr)
	}
	ms, mr := monthWindow(now)
	if ms != time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC) || mr != time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("month window wrong: %v..%v", ms, mr)
	}
}
