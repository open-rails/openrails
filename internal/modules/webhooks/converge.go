package webhooks

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #684: for the fetchable rails (Stripe, NMI) a verified webhook is a WAKE-UP
// SIGNAL, not a state source. The handler keeps four jobs — signature
// verification, dedup key, identifying the dirty object, the event timestamp —
// then marks the subscription dirty by enqueueing a coalesced fetch-and-
// converge job. All state writes flow through fetch → reconcile.Decide →
// ApplyDecision. CCBill has no read API and stays payload-apply (the
// documented exception).

// ConvergeRequest identifies one dirty subscription for fetch-and-converge.
type ConvergeRequest struct {
	MerchantID uuid.UUID
	// Rail is the canonical rail/provider key the event arrived on (the NMI
	// alias key selects the gateway client).
	Rail string
	// SubscriptionReference is the provider subscription id, or (NMI) an
	// order/PO reference that resolves to one — identity only, never state.
	SubscriptionReference string
	// EventType/EventCreated are carried for logging/forensics only; the fetch
	// decides from provider truth, so ordering is structurally irrelevant.
	EventType    string
	EventCreated int64
}

// SubscriptionConvergeEnqueuer schedules the coalesced (debounced, unique per
// subscription) fetch-and-converge job. Implemented by the River producer.
type SubscriptionConvergeEnqueuer interface {
	EnqueueSubscriptionConverge(ctx context.Context, req ConvergeRequest) error
}

// ErrConvergeRetryLater signals that provider evidence has not settled yet
// (e.g. an NMI initial charge still pending) — the converge job should snooze
// and re-fetch, not fail.
var ErrConvergeRetryLater = errors.New("subscription converge: provider evidence not settled; retry later")

// markSubscriptionDirty is the whole job of a slimmed webhook handler: attach
// the merchant, enqueue the coalesced fetch. An unavailable enqueuer is a
// RETRYABLE failure — the provider redelivers, nothing is lost.
func markSubscriptionDirty(ctx context.Context, enq SubscriptionConvergeEnqueuer, rail, reference, eventType string, eventCreated int64) error {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return fmt.Errorf("mark subscription dirty: empty subscription reference")
	}
	if enq == nil {
		return fmt.Errorf("mark subscription dirty: converge enqueuer unavailable (rail %s, event %s)", rail, eventType)
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return fmt.Errorf("mark subscription dirty: no merchant on context: %w", err)
	}
	if err := enq.EnqueueSubscriptionConverge(ctx, ConvergeRequest{
		MerchantID:            mid.UUID(),
		Rail:                  rail,
		SubscriptionReference: reference,
		EventType:             eventType,
		EventCreated:          eventCreated,
	}); err != nil {
		return fmt.Errorf("mark subscription dirty: enqueue converge: %w", err)
	}
	log.WithContext(ctx).WithFields(log.Fields{
		"rail": rail, "subscription_reference": reference, "event_type": eventType,
	}).Info("webhook marked subscription dirty; fetch-and-converge enqueued")
	return nil
}

// afterConvergeTransition lands the rail-agnostic post-transition effects the
// old payload-apply handlers carried:
//   - TransitionPastDue: payment-method-failed notification (best-effort).
//
// Renewal credits are part of the lifecycle transition's transaction; there
// is deliberately no post-transition credit write here.
func afterConvergeTransition(ctx context.Context, deps convergeDeps, sub *models.Subscription, res reconcile.SubscriptionConvergence) error {
	if !res.Applied {
		return nil
	}
	switch res.Decision.Kind {
	case reconcile.TransitionPastDue:
		if deps.NotificationService == nil {
			return nil
		}
		notification := &models.NotificationQueue{
			ID:         uuidutil.NewV7(),
			CustomerID: sub.CustomerID,
			EventType:  models.NotificationPaymentMethodFailed,
			Data: map[string]any{
				"rail":                 string(sub.Rail),
				"rail_subscription_id": sub.RailSubscriptionID,
				"source":               "fetch_converge",
			},
		}
		if err := deps.NotificationService.CreateAndDeliver(ctx, notification); err != nil {
			log.WithContext(ctx).WithError(err).WithField("subscription_id", sub.ID).
				Error("converge: failed to deliver payment failure notification")
		}
	}
	return nil
}

// convergeDeps are the shared service dependencies of the per-rail converge
// implementations.
type convergeDeps struct {
	NotificationService *subscriptions.NotificationService
}
