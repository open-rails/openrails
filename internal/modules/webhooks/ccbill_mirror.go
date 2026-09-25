package webhooks

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/billing/lifecycle"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	log "github.com/sirupsen/logrus"
)

// ccbillMirrorTransition applies one CCBill fact to a subscription the caller
// holds locked in d's transaction (#1094): the lifecycle machine decides, the
// row is persisted and the effects run in the same transaction. An outcome
// OpenRails decides (refund, void, chargeback) queues the CCBill cancel with
// it (#1102); one CCBill reported (notice.providerStopped) needs none.
// It reports whether the row changed and returns the customer notices to
// deliver after commit.
func (s *CCBillWebhookService) ccbillMirrorTransition(ctx context.Context, d *db.DB, sub *models.Subscription, ev lifecycle.Event, notice ccbillNotice) (bool, []*models.NotificationQueue, error) {
	now := s.now().UTC()
	before := *sub
	effects, err := subscriptions.Transition(sub, ev, now)
	if err != nil {
		return false, nil, err
	}
	changed := sub.Status != before.Status || !sameTime(sub.EndedAt, before.EndedAt) || !sameTime(sub.CurrentPeriodEndsAt, before.CurrentPeriodEndsAt)
	if !changed && len(effects) == 0 {
		return false, nil, nil
	}
	// Effects run through the shared executor; CCBill's notices keep
	// their handler context and are created at delivery.
	var access []lifecycle.Effect
	var notes []*models.NotificationQueue
	for _, e := range effects {
		switch e := e.(type) {
		case lifecycle.Notify:
			if n := notice.build(sub, e.Kind); n != nil {
				notes = append(notes, n)
			}
		case lifecycle.QueueProviderCancel:
			if !notice.providerStopped {
				access = append(access, e)
			}
		default:
			access = append(access, e)
		}
	}
	if _, err := s.lifecycleIn(d).ApplyEffects(ctx, d, sub, access, now, subscriptions.EffectOptions{RevokeReason: notice.revoke}); err != nil {
		return false, nil, err
	}
	if err := subscriptions.NewSubscriptionRepo(d).UpdateAt(ctx, sub, now); err != nil {
		return false, nil, fmt.Errorf("persist ccbill %s for %s: %w", lifecycle.Name(ev), sub.ID, err)
	}
	return true, notes, nil
}

// ccbillNotice carries the handler's context for the machine's effects.
type ccbillNotice struct {
	// providerStopped: CCBill itself ended the schedule.
	providerStopped bool
	revoke          models.EntitlementRevokeReason
	ended           subscriptions.PremiumEndReason
	data            openrails.NotificationData
}

func (n ccbillNotice) build(sub *models.Subscription, kind lifecycle.NoticeKind) *models.NotificationQueue {
	var event models.NotificationEventType
	data := n.data
	switch kind {
	case lifecycle.NoticeEnded:
		event, data.Reason = models.NotificationPremiumEnded, string(n.ended)
	case lifecycle.NoticePaymentFailed:
		event = models.NotificationPaymentMethodFailed
	default:
		return nil
	}
	return &models.NotificationQueue{ID: uuidutil.NewV7(), CustomerID: sub.CustomerID, EventType: event, Data: data}
}

func (s *CCBillWebhookService) deliver(ctx context.Context, notes []*models.NotificationQueue) {
	if s.NotificationService == nil {
		return
	}
	for _, n := range notes {
		if err := s.NotificationService.CreateAndDeliver(ctx, n); err != nil {
			log.WithContext(ctx).WithError(err).WithFields(log.Fields{"customer_id": n.CustomerID, "event": n.EventType}).Error("failed to deliver CCBill lifecycle notification")
		}
	}
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// ccbillReactivationUnappliedFinding: CCBill reactivated a membership the
// lifecycle cannot resume (no paid period left, or a chargeback).
const ccbillReactivationUnappliedFinding = "life.ccbill.reactivation_unapplied"

func raiseCCBillFinding(ctx context.Context, d *db.DB, sub *models.Subscription, findingType, action string, evidence map[string]any) error {
	evidence["subscription_id"] = openrails.SubscriptionID(sub.ID).String()
	evidence["rail_subscription_id"] = sub.RailSubscriptionID
	evidence["local_status"] = string(sub.Status)
	raw, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	_, err = d.Gen(ctx).UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
		MerchantID:        sub.MerchantID,
		FindingType:       findingType,
		SubjectKey:        "subscription:" + sub.ID.String(),
		Severity:          "medium",
		Status:            "requires_review",
		RecommendedAction: &action,
		Evidence:          raw,
	})
	return err
}

// lifecycleIn is the lifecycle service bound to the caller's transaction, so a
// core writer runs under the row lock the handler already holds.
func (s *CCBillWebhookService) lifecycleIn(d *db.DB) *subscriptions.SubscriptionLifecycleService {
	var notices *subscriptions.NotificationService
	if s.SubscriptionLifecycleService != nil {
		notices = s.SubscriptionLifecycleService.NotificationService
	}
	lc := subscriptions.NewSubscriptionLifecycleService(d, nil, nil, nil, notices, payments.NewPaymentService(d, s.Clock), s.Clock)
	if s.SubscriptionLifecycleService != nil {
		lc.SetConfig(s.SubscriptionLifecycleService.Config)
	}
	lc.SetProviderCancelScheduler(intents.NewProviderCancelScheduler(d, nil, intents.OriginAdmin, "CCBill reported a refund, void or chargeback; CCBill must stop rebilling"))
	return lc
}

// ccbillMirrorEvent locks the CCBill subscription, asks decide for the event
// (nil changes nothing) and applies it in one transaction.
func (s *CCBillWebhookService) ccbillMirrorEvent(ctx context.Context, railSubID string, decide func(*models.Subscription) lifecycle.Event, notice ccbillNotice) error {
	var notes []*models.NotificationQueue
	err := s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := db.NewWithPgxTx(tx)
		sub, err := subscriptions.NewSubscriptionRepo(d).GetByPSPSubscriptionIDForUpdate(ctx, string(models.RailCCBill), railSubID)
		if err != nil {
			if db.IsNotFound(err) {
				return fmt.Errorf("subscription not found for rail subscription ID: %s", railSubID)
			}
			return fmt.Errorf("failed to get subscription: %w", err)
		}
		ev := decide(sub)
		if ev == nil {
			return nil
		}
		_, notes, err = s.ccbillMirrorTransition(ctx, d, sub, ev, notice)
		return err
	})
	if err != nil {
		return err
	}
	s.deliver(ctx, notes)
	return nil
}
