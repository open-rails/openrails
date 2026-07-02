package intents

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// NMIDeleteScheduler implements subscriptions.DeferredDeleteScheduler on top
// of the provider intent ledger (#358 phase A, replacing the River delete job
// of issue 216): scheduling a deferred NMI delete ENQUEUES a durable
// nmi_delete_subscription intent, and cancelling one supersedes it. The
// DeletionScheduledAt marker on the subscription remains as the read model
// (set by the producers, cleared by the intent handler's finalize).
//
// origin distinguishes who asked: user-origin intents (a user cancel's
// undo-window delete) execute under mode=limited; system-origin intents
// (dunning exhaustion, unknown-resolution) require mode=full — see
// GateExecution. Queuing is UNCONDITIONAL (#679): mode gates execution only.
type NMIDeleteScheduler struct {
	db     *db.DB
	store  *Store
	origin Origin
	reason string
}

func NewNMIDeleteScheduler(d *db.DB, origin Origin, reason string) *NMIDeleteScheduler {
	return &NMIDeleteScheduler{db: d, store: NewStore(d), origin: origin, reason: reason}
}

// WithTx rebinds the scheduler onto the caller's transaction: the intent
// enqueue and the caller's subscription update commit or roll back together.
func (s *NMIDeleteScheduler) WithTx(tx pgx.Tx) subscriptions.DeferredDeleteScheduler {
	if s == nil {
		return s
	}
	txdb := s.db.NewWithPgxTx(tx)
	return &NMIDeleteScheduler{
		db:     txdb,
		store:  NewStore(txdb),
		origin: s.origin,
		reason: s.reason,
	}
}

// ScheduleNMIDelete enqueues the deferred delete intent, due at runAt.
// Idempotent per subscription (intents idempotency_key): repeated cancels of
// the same subscription refresh the pending intent instead of stacking
// duplicates; a re-cancel after a resume revives the superseded intent.
func (s *NMIDeleteScheduler) ScheduleNMIDelete(ctx context.Context, userID string, subscriptionID uuid.UUID, runAt time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("intent ledger unavailable for deferred delete scheduling")
	}
	// The subscription row carries the merchant + provider the intent needs.
	sub, err := s.db.Gen(ctx).GetSubscriptionByID(ctx, subscriptionID)
	if err != nil {
		return fmt.Errorf("load subscription for deferred delete intent: %w", err)
	}
	_, err = s.store.Enqueue(ctx, EnqueueParams{
		MerchantID:     sub.MerchantID,
		Provider:       strings.ToLower(sub.Rail),
		IntentType:     TypeNMIDeleteSubscription,
		SubscriptionID: &subscriptionID,
		Payload: NMIDeletePayload{
			UserID:             userID,
			RailSubscriptionID: sub.RailSubscriptionID,
		},
		IdempotencyKey: NMIDeleteIdempotencyKey(subscriptionID),
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
func (s *NMIDeleteScheduler) CancelNMIDelete(ctx context.Context, userID string, subscriptionID uuid.UUID) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.store.SupersedeBySubject(ctx, TypeNMIDeleteSubscription, subscriptionID,
		"cancellation undone (resume) for user "+userID)
	return err
}
