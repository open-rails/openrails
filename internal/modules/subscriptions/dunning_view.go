package subscriptions

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/lifecycle"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/collection"
)

// DunningViews reads the dunning of each subscription past_due or
// awaiting_method, by subscription id. prices holds the subscriptions'
// current prices by price id.
func DunningViews(ctx context.Context, d *db.DB, subs []*models.Subscription, prices map[uuid.UUID]*models.Price) (map[uuid.UUID]*billing.SubscriptionDunning, error) {
	out := map[uuid.UUID]*billing.SubscriptionDunning{}
	var ids []uuid.UUID
	var dues []time.Time
	for _, sub := range subs {
		if sub == nil || (sub.Status != models.StatusPastDue && sub.Status != models.StatusAwaitingMethod) {
			continue
		}
		view, err := dunningOf(ctx, d, sub, prices[sub.PriceID])
		if err != nil {
			return nil, err
		}
		out[sub.ID] = view
		if sub.CurrentPeriodEndsAt != nil {
			ids, dues = append(ids, sub.ID), append(dues, sub.CurrentPeriodEndsAt.UTC())
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := d.Gen(ctx).ListRenewalDeclines(ctx, gen.ListRenewalDeclinesParams{MerchantID: mid.UUID(), SubscriptionIds: ids, DueAts: dues, RowLimit: int32(len(ids))}) // #nosec G115 -- ids is at most one page (MaxPageLimit)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		view := out[row.SubscriptionID]
		// The counters miss a card-fix decline and the recorded attempts miss
		// imported history: each undercounts, so the case has made the larger.
		view.Attempts = max(view.Attempts, int(row.Declines))
		reason := billing.DeclineReason(row.Reason)
		view.LastFailureReason = &reason
	}
	return out, nil
}

// dunningOf is one case's dunning. OpenRails knows the schedule of the cases
// it collects (engine and NMI schedules) from the policy the case opened
// under. A provider runs its own retries: OpenRails holds at most its next
// attempt (CCBill reports it; Stripe's is not stored).
func dunningOf(ctx context.Context, d *db.DB, sub *models.Subscription, price *models.Price) (*billing.SubscriptionDunning, error) {
	failures := 0
	if sub.RetryAttempts != nil {
		failures = *sub.RetryAttempts
	}
	out := &billing.SubscriptionDunning{Attempts: failures + sub.TransientRetries, WaitingForNewCard: sub.Status == models.StatusAwaitingMethod}
	var next time.Time
	if sub.Status == models.StatusPastDue && sub.NextRetryAt != nil {
		next = sub.NextRetryAt.UTC()
		out.NextRetryAt = &next
	}
	if OwnerOf(sub) == lifecycle.Provider {
		return out, nil
	}
	policy, err := CasePolicy(ctx, d, sub)
	if err != nil {
		return nil, err
	}
	left, final, err := policy.Outlook(caseCycleHours(sub, price), failures, next)
	if errors.Is(err, collection.ErrUnknownCycle) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	out.RetriesLeft = &left
	switch {
	case out.WaitingForNewCard && sub.GraceEndsAt != nil:
		// No card, no charge: the case ends at the deadline.
		deadline := sub.GraceEndsAt.UTC()
		out.FinalRetryAt = &deadline
	case out.NextRetryAt != nil && left > 0:
		out.FinalRetryAt = &final
	}
	return out, nil
}

// caseCycleHours is the billing cycle a dunning case runs on: an engine
// member's current period, else its price's cadence.
func caseCycleHours(sub *models.Subscription, price *models.Price) int {
	if sub.CollectionPolicy == models.CollectionPolicyEngine && sub.CurrentPeriodStartsAt != nil && sub.CurrentPeriodEndsAt != nil {
		return collection.CycleHoursBetween(*sub.CurrentPeriodStartsAt, *sub.CurrentPeriodEndsAt)
	}
	return collection.BillingCycleHoursOf(price)
}
