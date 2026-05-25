package models

import (
	"testing"
	"time"
)

func TestSubscriptionClearRetrySchedule(t *testing.T) {
	now := time.Now().UTC()
	attempts := 3
	sub := &Subscription{
		LastRetryAt:   &now,
		RetryAttempts: &attempts,
		NextRetryAt:   &now,
		GraceEndsAt:   &now,
	}

	sub.ClearRetrySchedule()

	if sub.LastRetryAt != nil || sub.RetryAttempts != nil || sub.NextRetryAt != nil || sub.GraceEndsAt != nil {
		t.Fatalf("expected retry schedule fields to be cleared")
	}
}

func TestDefaultBillingCycleIs30Days(t *testing.T) {
	if got, want := DefaultBillingCycle, 30*24*time.Hour; got != want {
		t.Fatalf("DefaultBillingCycle = %v, want %v", got, want)
	}
}

func TestUpdateCurrentPeriodsFallsBackToDefaultCycle(t *testing.T) {
	priorEnd := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sub := &Subscription{CurrentPeriodEndsAt: &priorEnd}

	sub.updateCurrentPeriods(nil)

	if sub.CurrentPeriodStartsAt == nil || !sub.CurrentPeriodStartsAt.Equal(priorEnd) {
		t.Fatalf("CurrentPeriodStartsAt = %v, want %v", sub.CurrentPeriodStartsAt, priorEnd)
	}
	wantEnd := priorEnd.Add(DefaultBillingCycle)
	if sub.CurrentPeriodEndsAt == nil || !sub.CurrentPeriodEndsAt.Equal(wantEnd) {
		t.Fatalf("CurrentPeriodEndsAt = %v, want %v", sub.CurrentPeriodEndsAt, wantEnd)
	}
}

func TestUpdateCurrentPeriodsHonoursExplicitCycle(t *testing.T) {
	priorEnd := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sub := &Subscription{CurrentPeriodEndsAt: &priorEnd}
	cycle := 7 * 24 * time.Hour

	sub.updateCurrentPeriods(&cycle)

	wantEnd := priorEnd.Add(cycle)
	if sub.CurrentPeriodEndsAt == nil || !sub.CurrentPeriodEndsAt.Equal(wantEnd) {
		t.Fatalf("CurrentPeriodEndsAt = %v, want %v (explicit cycle must override default)", sub.CurrentPeriodEndsAt, wantEnd)
	}
}
