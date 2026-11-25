package subscriptions

import (
	"context"
	"time"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/collection"
	"github.com/open-rails/openrails/internal/modules/invoices/observed"
)

// NextRenewalAt is when the renewal after the one due at due comes due: the
// provider's own next charge for an NMI-scheduled subscription whose accepted
// schedule dates it, else due plus the cycle.
func NextRenewalAt(ctx context.Context, d *db.DB, sub *models.Subscription, due time.Time, cycleHours int) (time.Time, error) {
	if sub.CollectionPolicy == models.CollectionPolicyNMISchedule {
		if next, ok, err := observed.NextScheduledDue(ctx, d.Gen(ctx), sub, due); err != nil || ok {
			return next, err
		}
	}
	return due.Add(time.Duration(cycleHours) * time.Hour), nil
}

// CaseEnd is when sub's dunning case for the renewal due at due stops: its
// grace end when the network's advice deferred every retry past the next
// renewal, else the policy's (collection.Policy.CaseEnd).
func CaseEnd(ctx context.Context, d *db.DB, policy collection.Policy, sub *models.Subscription, due time.Time, cycleHours int) (time.Time, error) {
	if sub.AdviceDeferred() {
		return *sub.GraceEndsAt, nil
	}
	renewal, err := NextRenewalAt(ctx, d, sub, due, cycleHours)
	if err != nil {
		return time.Time{}, err
	}
	failures := 0
	if sub.RetryAttempts != nil {
		failures = *sub.RetryAttempts
	}
	return policy.CaseEnd(cycleHours, due, renewal, failures, sub.NextRetryAt)
}
