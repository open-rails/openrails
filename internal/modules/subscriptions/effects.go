package subscriptions

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/lifecycle"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/providerrecovery"
)

// EffectOptions is the caller's context for a transition's effects.
type EffectOptions struct {
	// RevokeReason labels ended access; empty derives it from the cancel type.
	RevokeReason models.EntitlementRevokeReason
	// EndedReason is the premium_ended notice's reason.
	EndedReason PremiumEndReason
	// Notice is the base data of every customer notice.
	Notice billing.NotificationData
}

// ApplyEffects carries out a transition's effects inside the caller's
// transaction (#1089 §3), on the row the caller holds locked. Call it after
// Transition and before persisting the row: a provider cancel stamps the
// row's deletion marker. Notices are queued idempotently and returned for the
// caller to deliver after commit (DispatchNotifications).
//
// Transition owns retry fields; these effects project the corresponding grace
// policy. The unverified trigger separately wakes provider resolution on commit.
func (s *SubscriptionLifecycleService) ApplyEffects(ctx context.Context, d *db.DB, sub *models.Subscription, effects []lifecycle.Effect, now time.Time, opts EffectOptions) ([]*models.NotificationQueue, error) {
	for _, effect := range effects {
		switch effect.(type) {
		case lifecycle.EndAccess, lifecycle.QueueProviderCancel:
			if err := s.CheckCancellationRecovery(ctx, d, sub, now); err != nil {
				return nil, err
			}
		}
	}
	ents := s.newLifecycleEntitlementService(d)
	var out []*models.NotificationQueue
	var granted *lifecycle.GrantPeriod
	for _, effect := range effects {
		switch e := effect.(type) {
		case lifecycle.GrantPeriod:
			granted = &e
			// A paid period replaces every prior renewal allowance, including
			// benefits removed by a scheduled tier change. Purchased grants keep
			// their own independent expiry.
			if err := ents.RevokeSourcesForSubscriptionAsOf(ctx, sub.CustomerID.String(), sub.ID, e.Start, models.EntitlementRevokeSuperseded, models.EntitlementSourceGrace); err != nil {
				return nil, fmt.Errorf("end superseded renewal grace %s: %w", sub.ID, err)
			}
			for name := range sub.EntitlementsSpecSnapshot {
				if _, err := ents.PushNewEntitlement(ctx, subscriptionAccess(sub, name, e.Start)); err != nil {
					return nil, fmt.Errorf("grant period %s %s: %w", sub.ID, name, err)
				}
			}
			if err := pushRenewalGrace(ctx, d, ents, sub, entitlementNames(sub.EntitlementsSpecSnapshot), e.Start, e.End); err != nil {
				return nil, err
			}

		case lifecycle.EndAccess:
			// Canceling recurrence cannot shorten access already purchased.
			sources := []models.EntitlementSourceType{models.EntitlementSourceGrace}
			if e.Revoke {
				sources = append(sources, models.EntitlementSourceSubscription)
			}
			if err := ents.RevokeSourcesForSubscriptionAsOf(ctx, sub.CustomerID.String(), sub.ID, now, revokeReason(sub, opts), sources...); err != nil {
				return nil, fmt.Errorf("end access %s: %w", sub.ID, err)
			}
			if sub.Rail == models.RailSolana {
				if err := s.cancelSolanaSubscriptionForLifecycle(ctx, d, sub.ID); err != nil {
					return nil, fmt.Errorf("end access %s: cancel Solana subscription: %w", sub.ID, err)
				}
			}
		case lifecycle.QueueProviderCancel:
			if err := s.queueProviderCancel(ctx, d, sub, now); err != nil {
				return nil, err
			}
		case lifecycle.OpenDunning:
			if err := s.dunningAccess(ctx, d, ents, sub, now); err != nil {
				return nil, err
			}
		case lifecycle.ProbeProvider, lifecycle.ReopenAccess:
			if sub.CurrentPeriodStartsAt != nil && sub.CurrentPeriodEndsAt != nil {
				if err := pushRenewalGrace(ctx, d, ents, sub, entitlementNames(sub.EntitlementsSpecSnapshot), *sub.CurrentPeriodStartsAt, *sub.CurrentPeriodEndsAt); err != nil {
					return nil, err
				}
			}
		case lifecycle.CloseDunning:
			// Paid grants retain their own expiry when billing resumes.

		case lifecycle.Notify:
			n, err := s.queueNotice(ctx, d, sub, e.Kind, granted, opts)
			if err != nil {
				return nil, err
			}
			if n != nil {
				out = append(out, n)
			}
		}
	}
	return out, nil
}

// queueProviderCancel queues, in d's transaction, the cancel of the provider
// schedule a terminal transition leaves billing (#1102). An NMI delete waits
// out the system cooling-off window; a replay finds it already queued.
func (s *SubscriptionLifecycleService) queueProviderCancel(ctx context.Context, d *db.DB, sub *models.Subscription, now time.Time) error {
	if sub.RailSubscriptionID == "" {
		return nil
	}
	if s.providerCancel == nil {
		log.WithContext(ctx).WithFields(log.Fields{"subscription_id": sub.ID, "rail": sub.Rail}).
			Warn("no provider-cancel scheduler wired: the provider cancel is NOT queued (wiring gap)")
		return nil
	}
	if rails.RemoteDeleteOnTerminalCancel(sub.Rail) {
		if sub.DeletionScheduledAt != nil {
			return nil
		}
		at := SystemDeferredDeleteAt(sub, now)
		sub.DeletionScheduledAt = &at
	}
	if err := d.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return s.providerCancel.WithTx(tx).ScheduleProviderCancel(ctx, sub, now)
	}); err != nil {
		return fmt.Errorf("queue provider cancel %s: %w", sub.ID, err)
	}
	return nil
}

func revokeReason(sub *models.Subscription, opts EffectOptions) models.EntitlementRevokeReason {
	switch {
	case opts.RevokeReason != "":
		return opts.RevokeReason
	case sub.CancelType != nil && *sub.CancelType == models.CancelTypeChargeback:
		return models.EntitlementRevokeChargeback
	default:
		return models.EntitlementRevokeDunning
	}
}

// queueNotice queues one customer notice, idempotent per subscription, kind
// and period so a replayed transition never notifies twice.
func (s *SubscriptionLifecycleService) queueNotice(ctx context.Context, d *db.DB, sub *models.Subscription, kind lifecycle.NoticeKind, granted *lifecycle.GrantPeriod, opts EffectOptions) (*models.NotificationQueue, error) {
	data := opts.Notice
	data.SubscriptionID = billing.SubscriptionID(sub.ID)
	if data.Rail == "" {
		data.Rail, data.RailSubscriptionID = string(sub.Rail), sub.RailSubscriptionID
	}
	if kind == lifecycle.NoticeEnded && opts.EndedReason != "" {
		data.Reason = string(opts.EndedReason)
	}
	key := string(kind)
	if sub.CurrentPeriodEndsAt != nil {
		key += ":" + sub.CurrentPeriodEndsAt.UTC().Format(time.RFC3339Nano)
	}
	if sub.EndedAt != nil {
		key += ":" + sub.EndedAt.UTC().Format(time.RFC3339Nano)
	}
	if granted != nil {
		start, end := granted.Start, granted.End
		data.PeriodStartsAt, data.PeriodEndsAt = &start, &end
		if kind == lifecycle.NoticeRenewed {
			due, err := renewalReceiptDue(ctx, d, sub, start)
			if err != nil || !due {
				return nil, err
			}
			key = "renewed:" + start.UTC().Format(time.RFC3339Nano) + ":" + end.UTC().Format(time.RFC3339Nano)
		}
	}
	n := &models.NotificationQueue{ID: uuid.NewSHA1(sub.ID, []byte(key)), CustomerID: sub.CustomerID, EventType: models.NotificationEventType(kind), Data: data}
	if err := NewNotificationQueueRepo(d).CreateIfAbsent(ctx, n); err != nil {
		return nil, fmt.Errorf("queue %s notice for %s: %w", kind, sub.ID, err)
	}
	return n, nil
}

// ApplyScheduledTier opens a scheduled period-end tier change on a provider
// renewal of an NMI schedule: the provider already bills the scheduled price,
// so the renewed period is the new tier's. Call after a RenewalPaid
// transition, before persisting the row.
func (s *SubscriptionLifecycleService) ApplyScheduledTier(ctx context.Context, d *db.DB, sub *models.Subscription) error {
	if sub.CollectionPolicy == models.CollectionPolicyEngine || !rails.IsNMI(sub.Rail) || sub.ScheduledPriceID == nil ||
		sub.CurrentPeriodStartsAt == nil || sub.CurrentPeriodEndsAt == nil {
		return nil
	}
	price, err := catalog.NewPriceService(d).GetByID(ctx, *sub.ScheduledPriceID)
	if err != nil {
		return fmt.Errorf("scheduled tier %s: price: %w", sub.ID, err)
	}
	product, err := catalog.NewProductService(d).GetByID(ctx, price.ProductID)
	if err != nil {
		return fmt.Errorf("scheduled tier %s: product: %w", sub.ID, err)
	}
	sub.PriceID, sub.ProductID, sub.ScheduledPriceID = price.ID, product.ID, nil
	sub.EntitlementsSpecSnapshot = models.CloneEntitlementsSpec(product.EntitlementsSpec)
	sub.AccessDurationHoursSnapshot = price.AccessDurationHours
	sub.Price = price
	return nil // The caller grants the newly paid period from this snapshot.
}

// CheckCancellationRecovery keeps automated local revocation and its eventual
// provider delete behind the same recovered-account boundary. A positive paid
// renewal has no cancellation effect and can still settle while writes hold.
func (s *SubscriptionLifecycleService) CheckCancellationRecovery(ctx context.Context, d *db.DB, sub *models.Subscription, now time.Time) error {
	if s.Config != nil && config.IsProviderReadOnly(s.Config) {
		return fmt.Errorf("%w: readonly holds local cancellation", providerrecovery.ErrPending)
	}
	if sub.PspID == uuid.Nil {
		return nil
	}
	return providerrecovery.CheckPSP(ctx, d, sub.MerchantID, sub.PspID, now)
}
