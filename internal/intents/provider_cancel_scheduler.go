package intents

import (
	"github.com/open-rails/openrails/pkg/merchant"

	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// ProviderCancelScheduler implements subscriptions.ProviderCancelScheduler on
// the provider intent ledger: every provider-owned schedule OpenRails ends
// locally is stopped by a durable intent enqueued in the same transaction —
// the NMI deferred delete, the CCBill DataLink cancel, the Stripe cancel. Each
// is idempotent per subscription. The DeletionScheduledAt marker remains the
// NMI read model (set here, cleared by the intent handler's finalize).
//
// origin distinguishes who asked: user-origin intents execute under
// mode=limited; system-origin intents (dunning exhaustion, unknown-resolution)
// require mode=full — see GateExecution. Queuing is UNCONDITIONAL (#679): mode
// gates execution only.
type ProviderCancelScheduler struct {
	db     *db.DB
	store  *Store
	origin Origin
	reason string
}

// NewProviderCancelScheduler builds the scheduler. ceiling (may be nil) is the
// #732 rate ceiling; user/admin-origin schedulers must pass it so self-service
// and admin cancels are gated.
func NewProviderCancelScheduler(d *db.DB, ceiling *RateCeiling, origin Origin, reason string) *ProviderCancelScheduler {
	return &ProviderCancelScheduler{db: d, store: NewStoreGated(d, ceiling), origin: origin, reason: reason}
}

// WithTx rebinds the scheduler onto the caller's transaction: the intent
// enqueue and the caller's subscription update commit or roll back together.
// The rate ceiling (its own pool-backed DB) is preserved across the rebind.
func (s *ProviderCancelScheduler) WithTx(tx pgx.Tx) subscriptions.ProviderCancelScheduler {
	if s == nil {
		return s
	}
	txdb := s.db.NewWithPgxTx(tx)
	return &ProviderCancelScheduler{db: txdb, store: s.store.withTxDB(txdb), origin: s.origin, reason: s.reason}
}

// ScheduleProviderCancel queues the cancel of sub's provider schedule, if a
// provider bills it. An NMI delete is due at sub.DeletionScheduledAt (the
// caller's undo or cooling-off window), else now, and stamps the marker;
// Stripe and CCBill cancels are due now.
func (s *ProviderCancelScheduler) ScheduleProviderCancel(ctx context.Context, sub *models.Subscription, now time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("intent ledger unavailable for provider cancel scheduling")
	}
	if sub == nil || strings.TrimSpace(sub.RailSubscriptionID) == "" {
		return nil
	}
	switch {
	case rails.RemoteDeleteOnTerminalCancel(sub.Rail):
		at := now
		if sub.DeletionScheduledAt != nil {
			at = *sub.DeletionScheduledAt
		}
		sub.DeletionScheduledAt = &at
		return s.ScheduleNMIDelete(ctx, sub.CustomerID.String(), sub.ID, at)
	case sub.Rail == models.RailCCBill:
		return s.enqueue(ctx, sub, TypeCCBillCancelSubscription, CCBillCancelIdempotencyKey(sub.ID),
			CCBillCancelPayload{UserID: sub.CustomerID.String(), RailSubscriptionID: sub.RailSubscriptionID}, now)
	case StripeOwned(sub):
		return s.enqueue(ctx, sub, TypeStripeCancelSubscription, StripeCancelIdempotencyKey(sub.ID),
			StripeCancelPayload{RailSubscriptionID: sub.RailSubscriptionID}, now)
	}
	return nil
}

// enqueue writes one remote-cancel intent, addressed (or#893) to the PSP
// account that holds the subscription.
func (s *ProviderCancelScheduler) enqueue(ctx context.Context, sub *models.Subscription, intentType, key string, payload any, due time.Time) error {
	scope, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	_, err = s.store.Enqueue(ctx, EnqueueParams{
		MerchantID:     scope.UUID(),
		Provider:       strings.ToLower(string(sub.Rail)),
		IntentType:     intentType,
		SubscriptionID: &sub.ID,
		PspID:          sub.PspID,
		Payload:        payload,
		IdempotencyKey: key,
		NextAttemptAt:  due.UTC(),
		Origin:         s.origin,
		OriginReason:   s.reason,
	})
	return err
}

// ScheduleNMIDelete enqueues the deferred delete intent, due at runAt.
// Idempotent per subscription (intents idempotency_key): repeated cancels of
// the same subscription refresh the pending intent instead of stacking
// duplicates; a re-cancel after a resume revives the superseded intent.
func (s *ProviderCancelScheduler) ScheduleNMIDelete(ctx context.Context, userID string, subscriptionID uuid.UUID, runAt time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("intent ledger unavailable for deferred delete scheduling")
	}
	// The subscription row carries the merchant, provider and — or#893 — the PSP
	// the intent must execute against. The delete is addressed to the SAME
	// gateway account that holds the schedule; anything else would fire against
	// a sibling account's book.
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return scopeErr
	}
	sub, err := s.db.Gen(ctx).GetSubscriptionByID(ctx, gen.GetSubscriptionByIDParams{MerchantID: scopeMerchantID.UUID(), ID: subscriptionID})
	if err != nil {
		return fmt.Errorf("load subscription for deferred delete intent: %w", err)
	}
	_, err = s.store.Enqueue(ctx, EnqueueParams{
		MerchantID:     sub.MerchantID,
		Provider:       strings.ToLower(sub.Rail),
		IntentType:     TypeNMIDeleteSubscription,
		SubscriptionID: &subscriptionID,
		PspID:          sub.PspID,
		Payload: NMIDeletePayload{
			UserID:             userID,
			RailSubscriptionID: sub.RailSubscriptionID,
		},
		IdempotencyKey: NMIDeleteIdempotencyKey(subscriptionID, sub.PspID, sub.RailSubscriptionID),
		NextAttemptAt:  runAt.UTC(),
		Origin:         s.origin,
		OriginReason:   s.reason,
	})
	return err
}

// CancelNMIDelete supersedes any live deferred-delete intent for the
// subscription. Advisory only: the intent handler's relevance check re-reads
// the subscription state and supersedes on its own if the cancellation was
// resumed, so a missed supersede here cannot cause an erroneous delete.
func (s *ProviderCancelScheduler) CancelNMIDelete(ctx context.Context, userID string, subscriptionID uuid.UUID) error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.db.NewWithPgxTx(tx)
		sub, err := subscriptions.NewSubscriptionRepo(d).GetByIDForUpdate(ctx, subscriptionID)
		if err != nil {
			return err
		}
		reason := "cancellation undone (resume) for user " + userID
		_, err = d.Gen(ctx).SupersedePendingNMIDelete(ctx, gen.SupersedePendingNMIDeleteParams{MerchantID: sub.MerchantID, IdempotencyKey: NMIDeleteIdempotencyKey(subscriptionID, sub.PspID, sub.RailSubscriptionID), Reason: &reason})
		return err
	})
}
