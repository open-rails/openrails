package subscriptions

import (
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/lifecycle"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
)

// OwnerOf maps a subscription's collection policy to the state machine's
// owner (design §2): who charges renewals and who retries declines.
func OwnerOf(sub *models.Subscription) lifecycle.Owner {
	switch {
	case sub.CollectionPolicy == models.CollectionPolicyEngine:
		return lifecycle.Engine
	case rails.IsNMI(sub.Rail):
		return lifecycle.NMISchedule
	default:
		return lifecycle.Provider
	}
}

// SnapshotOf is the part of the row the state machine decides on.
func SnapshotOf(sub *models.Subscription) lifecycle.Snapshot {
	s := lifecycle.Snapshot{Status: lifecycle.Status(sub.Status), Owner: OwnerOf(sub)}
	if sub.CurrentPeriodEndsAt != nil {
		s.PaidThrough = sub.CurrentPeriodEndsAt.UTC()
	}
	if sub.EndedAt != nil {
		s.EndedAt = sub.EndedAt.UTC()
	} else if sub.Status == models.StatusCanceled {
		s.EndedAt = s.PaidThrough // an older period-end cancel recorded no end
	}
	if sub.CancelType != nil {
		s.CancelKind = cancelKindOf(*sub.CancelType)
	}
	return s
}

// Transition applies one lifecycle event to a subscription the caller holds
// locked, writing the decided state onto the row (not yet persisted). The
// caller persists the row and carries out the returned effects in the same
// transaction. A replayed or stale fact returns no effects and leaves the row
// unchanged.
func Transition(sub *models.Subscription, ev lifecycle.Event, now time.Time) ([]lifecycle.Effect, error) {
	before := SnapshotOf(sub)
	next, effects, err := lifecycle.Apply(before, ev)
	if err != nil {
		return nil, err
	}
	if next != before {
		writeSnapshot(sub, before, next, effects, now)
		sub.MarkLifecycleDecision(lifecycle.Name(ev))
	}
	for _, e := range effects {
		if _, ok := e.(lifecycle.CloseDunning); ok {
			if next.Status == lifecycle.Active || next.Status == lifecycle.Canceled {
				sub.ClearRetrySchedule()
			} else {
				sub.NextRetryAt, sub.GraceEndsAt = nil, nil // attempts stay as evidence
			}
		}
	}
	if next.Status == lifecycle.Active || next.Status == lifecycle.Canceled {
		sub.DunningPolicy = nil // the dunning case is over
	}
	switch next.Status {
	case lifecycle.Canceled, lifecycle.Unverified:
		sub.NextRetryAt, sub.GraceEndsAt = nil, nil
	case lifecycle.AwaitingMethod:
		sub.NextRetryAt = nil // nothing is charged; GraceEndsAt is the deadline
	}
	return effects, nil
}

func writeSnapshot(sub *models.Subscription, before, next lifecycle.Snapshot, effects []lifecycle.Effect, now time.Time) {
	sub.Status = models.SubscriptionStatus(next.Status)
	if !next.PaidThrough.Equal(before.PaidThrough) {
		end := next.PaidThrough
		sub.CurrentPeriodEndsAt = &end
		for _, e := range effects {
			if g, ok := e.(lifecycle.GrantPeriod); ok {
				start := g.Start
				sub.CurrentPeriodStartsAt = &start
			}
		}
	}
	switch {
	case next.Status == lifecycle.Canceled && before.Status != lifecycle.Canceled:
		kind := modelCancelType(next.CancelKind)
		ended, canceled := next.EndedAt, now
		if ended.Before(canceled) {
			canceled = ended
		}
		sub.CancelType, sub.EndedAt, sub.CanceledAt = &kind, &ended, &canceled
	case next.Status == lifecycle.Canceled:
		kind, ended := modelCancelType(next.CancelKind), next.EndedAt
		sub.CancelType, sub.EndedAt = &kind, &ended
		if sub.CanceledAt != nil && sub.CanceledAt.After(ended) {
			sub.CanceledAt = &ended
		}
	case before.Status == lifecycle.Canceled:
		// A pending provider stop is superseded: the schedule bills a live row.
		sub.CancelType, sub.EndedAt, sub.CanceledAt, sub.CancelFeedback, sub.DeletionScheduledAt = nil, nil, nil, nil, nil
	}
}

func cancelKindOf(t models.CancelType) lifecycle.CancelKind {
	switch t {
	case models.CancelTypeUser:
		return lifecycle.CancelUser
	case models.CancelTypeMerchant:
		return lifecycle.CancelMerchant
	case models.CancelTypeChargeback:
		return lifecycle.CancelChargeback
	case models.CancelTypeExpired:
		return lifecycle.CancelExpired
	default:
		return lifecycle.CancelKind(t)
	}
}

// modelCancelType stores a provider-confirmed or abandoned end as expired,
// the stored vocabulary's terminal non-customer reason.
func modelCancelType(k lifecycle.CancelKind) models.CancelType {
	switch k {
	case lifecycle.CancelProvider, lifecycle.CancelAbandoned:
		return models.CancelTypeExpired
	default:
		return models.CancelType(k)
	}
}
