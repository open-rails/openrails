package models

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/pkg/identity"
)

// satisfiesEndedNotBeforeCancelled mirrors the DB CHECK constraint
// chk_ended_not_before_cancelled defined in migrations/postgres/001_schema.up.sql:
//
//	ended_at IS NULL OR cancelled_at IS NULL OR ended_at >= cancelled_at
func satisfiesEndedNotBeforeCancelled(s *Subscription) bool {
	if s.EndedAt == nil || s.CancelledAt == nil {
		return true
	}
	return !s.EndedAt.Before(*s.CancelledAt)
}

// newExpiredSubscription builds an active subscription whose current period has
// already ended, the precondition under which ResetCurrentPeriods stamps ended_at.
func newExpiredSubscription() *Subscription {
	past := time.Now().Add(-time.Hour)
	start := past.Add(-30 * 24 * time.Hour)
	return &Subscription{
		ID:                    uuid.New(),
		CustomerID:            identity.CustomerIDFromString("user-1").UUID(),
		Status:                StatusActive,
		StartedAt:             start,
		CurrentPeriodStartsAt: &start,
		CurrentPeriodEndsAt:   &past,
	}
}

func TestResetCurrentPeriodsNoWallClockFudge(t *testing.T) {
	sub := newExpiredSubscription()

	before := time.Now()
	if err := sub.ResetCurrentPeriods(); err != nil {
		t.Fatalf("ResetCurrentPeriods returned error: %v", err)
	}
	after := time.Now()

	if sub.EndedAt == nil {
		t.Fatalf("expected ended_at to be set for an expired subscription")
	}

	// ended_at must be the true "now" instant, not an artificially
	// back-dated time (the old code subtracted 2 minutes).
	if sub.EndedAt.Before(before) || sub.EndedAt.After(after) {
		t.Fatalf("ended_at %v should fall within [%v, %v]; it must not be fudged backwards",
			sub.EndedAt, before, after)
	}
	if before.Sub(*sub.EndedAt) > time.Second {
		t.Fatalf("ended_at %v is suspiciously far behind now %v (looks like a wall-clock subtraction)",
			sub.EndedAt, before)
	}
}

func TestCancelSatisfiesEndedNotBeforeCancelled(t *testing.T) {
	sub := newExpiredSubscription()

	ct := CancelTypeUser
	if err := sub.Cancel("done", &ct); err != nil {
		t.Fatalf("Cancel returned error: %v", err)
	}

	if sub.CancelledAt == nil {
		t.Fatalf("expected cancelled_at to be set")
	}
	if sub.EndedAt == nil {
		t.Fatalf("expected ended_at to be set")
	}
	if !satisfiesEndedNotBeforeCancelled(sub) {
		t.Fatalf("constraint violated: ended_at %v < cancelled_at %v", sub.EndedAt, sub.CancelledAt)
	}
	if sub.Status != StatusCancelled {
		t.Fatalf("expected status cancelled, got %v", sub.Status)
	}
}

func TestCancelImmediatelyAfterCreateHoldsOrdering(t *testing.T) {
	// A subscription cancelled the instant after creation: its period has
	// effectively just ended, so ended_at and cancelled_at land at ~the same
	// moment. The ordering must still hold by construction.
	now := time.Now()
	sub := &Subscription{
		ID:                    uuid.New(),
		CustomerID:            identity.CustomerIDFromString("user-2").UUID(),
		Status:                StatusActive,
		StartedAt:             now,
		CurrentPeriodStartsAt: &now,
		CurrentPeriodEndsAt:   &now, // ends right now
	}

	ct := CancelTypeMerchant
	if err := sub.Cancel("immediate", &ct); err != nil {
		t.Fatalf("Cancel returned error: %v", err)
	}

	if !satisfiesEndedNotBeforeCancelled(sub) {
		t.Fatalf("constraint violated on immediate cancel: ended_at %v < cancelled_at %v",
			sub.EndedAt, sub.CancelledAt)
	}
}
