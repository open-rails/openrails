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
