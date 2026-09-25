package reconcile

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/billing/lifecycle"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/collection"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/normalize"
)

// ApplyDecision moves subscription state for one decider transition (#665)
// through the lifecycle state machine (#1091): the row is locked, re-checked
// against the row the decision was made on, and the decision's event is
// applied with Transition and ApplyEffects in one transaction. Only a payment
// fact renews and only a decline fact opens dunning. Returns whether a
// transition was attempted.
func ApplyDecision(ctx context.Context, database *db.DB, lc *subscriptions.SubscriptionLifecycleService, sub *models.Subscription, d Decision, now time.Time) (bool, error) {
	if database == nil || lc == nil || sub == nil {
		return false, fmt.Errorf("apply decision: db, lifecycle and subscription are required")
	}
	if d.Kind == TransitionNone || d.stale(sub) {
		return false, nil
	}
	switch d.Kind {
	case TransitionParkUnknown:
		if sub.Status != models.StatusActive && sub.Status != models.StatusPastDue {
			return false, nil
		}
		return true, lc.ApplyLocalUnknown(ctx, database, sub)

	case TransitionPastDue:
		if DunsDecline(sub, d) {
			return applyDunnedDecline(ctx, lc, sub, d)
		}
		if d.Decline == nil && subscriptions.OwnerOf(sub) != lifecycle.Provider {
			// A stalled provider date is no decline: the row is verified
			// instead of dunned (#1089 §1).
			if sub.Status != models.StatusActive {
				return false, nil
			}
			return true, lc.ApplyLocalUnknown(ctx, database, sub, models.StatusActive)
		}
		at := now
		if d.Decline != nil && !d.Decline.OccurredAt.IsZero() {
			at = d.Decline.OccurredAt
		}
		return transition(ctx, database, lc, sub, d, now, func(cur *models.Subscription) lifecycle.Event {
			return lifecycle.RenewalDeclined{PeriodStart: paidThroughOf(cur), Bucket: lifecycle.Retry, At: at}
		})

	case TransitionRenew:
		if d.NewPeriodEnd == nil {
			return false, nil
		}
		return transition(ctx, database, lc, sub, d, now, func(cur *models.Subscription) lifecycle.Event {
			start := paidThroughOf(cur)
			if start.IsZero() {
				start = now
				if d.NewPeriodStart != nil && d.NewPeriodStart.Before(*d.NewPeriodEnd) {
					start = d.NewPeriodStart.UTC()
				}
			}
			return lifecycle.RenewalPaid{PeriodStart: start, PeriodEnd: d.NewPeriodEnd.UTC()}
		})

	case TransitionAdoptPeriodEnd:
		return applyAdoption(ctx, database, lc, sub, d, now)

	case TransitionCancel:
		at := d.EvidenceAt
		if at.IsZero() {
			at = now
		}
		return transition(ctx, database, lc, sub, d, now, func(cur *models.Subscription) lifecycle.Event {
			switch {
			case d.RemoteGone:
				return lifecycle.ProviderCancelled{At: at}
			case d.Certainty == collection.CertaintyNonRetryableDecline:
				return lifecycle.RenewalDeclined{PeriodStart: paidThroughOf(cur), Bucket: lifecycle.NonRecoverable, At: at}
			default:
				return lifecycle.DunningExhausted{At: at}
			}
		})
	}
	return false, fmt.Errorf("apply decision: unknown transition %d", d.Kind)
}

// applyDunnedDecline hands a seen decline of an OpenRails-dunned NMI schedule
// to FailMembership, the dunning writer, which classifies it and schedules
// the next attempt (never "now"). A decline already counted is ignored.
func applyDunnedDecline(ctx context.Context, lc *subscriptions.SubscriptionLifecycleService, sub *models.Subscription, d Decision) (bool, error) {
	if sub.NextRetryAt != nil || (sub.LastRetryAt != nil && !d.Decline.OccurredAt.After(*sub.LastRetryAt)) {
		return false, nil
	}
	code := normalize.Trim(d.Decline.DeclineCode)
	return true, lc.FailMembership(ctx, &subscriptions.FailMembershipParams{
		Rail:            sub.Rail,
		SubscriptionID:  &sub.ID,
		FailureCode:     normalize.OptionalString(code),
		Decline:         collection.ClassifyDecline(string(sub.Rail), code),
		AttemptRecorded: true,
	})
}

// applyAdoption keeps the provider's future boundary for a row with no
// charge. Retired for ProviderConfirmedCurrent once the core writers land.
func applyAdoption(ctx context.Context, database *db.DB, lc *subscriptions.SubscriptionLifecycleService, sub *models.Subscription, d Decision, now time.Time) (bool, error) {
	if sub.Status != models.StatusUnverified {
		if sub.Status != models.StatusActive {
			return false, nil
		}
		if err := lc.ApplyLocalUnknown(ctx, database, sub, models.StatusActive); err != nil {
			return false, err
		}
	}
	return true, lc.ResolveUnknownSubscription(ctx, database, sub, subscriptions.ResolveAdopted, d.NewPeriodStart, d.NewPeriodEnd, now)
}

// transition applies one event to the locked row and carries out its
// effects in the same transaction; notifications go out after commit.
func transition(ctx context.Context, database *db.DB, lc *subscriptions.SubscriptionLifecycleService, sub *models.Subscription, d Decision, now time.Time, event func(*models.Subscription) lifecycle.Event) (bool, error) {
	var notices []*models.NotificationQueue
	applied := false
	err := database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txdb := database.NewWithPgxTx(tx)
		repo := subscriptions.NewSubscriptionRepo(txdb)
		cur, err := repo.GetByIDForUpdate(ctx, sub.ID)
		if err != nil {
			return fmt.Errorf("apply decision: lock %s: %w", sub.ID, err)
		}
		if d.stale(cur) {
			return nil
		}
		before := subscriptions.SnapshotOf(cur)
		ev := event(cur)
		effects, err := subscriptions.Transition(cur, ev, now)
		if err != nil {
			if errors.Is(err, lifecycle.ErrIllegal) || errors.Is(err, lifecycle.ErrInvalid) || errors.Is(err, lifecycle.ErrTerminal) {
				log.WithContext(ctx).WithError(err).WithFields(log.Fields{"subscription_id": cur.ID, "reason": d.Reason}).
					Warn("apply decision: the lifecycle refused the event; the row is unchanged")
				return nil
			}
			return err
		}
		if len(effects) == 0 && subscriptions.SnapshotOf(cur) == before {
			return nil
		}
		if notices, err = lc.ApplyEffects(ctx, tx, cur, customerNotices(effects, d.Silent), now); err != nil {
			return err
		}
		if _, renewed := ev.(lifecycle.RenewalPaid); renewed {
			if err := lc.ApplyScheduledTier(ctx, tx, cur); err != nil {
				return err
			}
		}
		if cur.Status == models.StatusCancelled && before.Status != lifecycle.Cancelled {
			feedback := "provider-confirmed: " + d.Reason
			cur.CancelFeedback = &feedback
		}
		if err := repo.UpdateAt(ctx, cur, now); err != nil {
			return fmt.Errorf("apply decision: update %s: %w", cur.ID, err)
		}
		*sub, applied = *cur, true
		return nil
	})
	if err != nil {
		return false, err
	}
	lc.DispatchNotifications(ctx, notices)
	return applied, nil
}

func paidThroughOf(sub *models.Subscription) time.Time {
	if sub.CurrentPeriodEndsAt == nil {
		return time.Time{}
	}
	return sub.CurrentPeriodEndsAt.UTC()
}

// customerNotices keeps a mirrored transition's notices, except the end of
// access, which the converge NOTIFY pass sends once for every ending (#789),
// and every notice when the decision replays declared history.
func customerNotices(effects []lifecycle.Effect, silent bool) []lifecycle.Effect {
	out := effects[:0:0]
	for _, e := range effects {
		if n, ok := e.(lifecycle.Notify); ok && (silent || n.Kind == lifecycle.NoticeEnded) {
			continue
		}
		out = append(out, e)
	}
	return out
}
