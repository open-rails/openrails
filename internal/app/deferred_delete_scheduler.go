package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments/processors"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// intentDeferredDeleteScheduler implements subscriptions.DeferredDeleteScheduler
// on top of the provider intent ledger (#358 phase A, replacing the River
// delete job of issue 216): scheduling a deferred NMI delete ENQUEUES a
// durable nmi_delete_subscription intent, and cancelling one supersedes it.
// The DeletionScheduledAt marker on the subscription remains as the read
// model (set by the producers, cleared by the intent handler's finalize).
//
// origin distinguishes who asked: user-origin intents (a user cancel's
// undo-window delete) execute under mode=limited; system-origin intents
// (dunning exhaustion) require mode=full — see intents.GateExecution.
type intentDeferredDeleteScheduler struct {
	db     *db.DB
	store  *intents.Store
	origin intents.Origin
	reason string
}

func newIntentDeferredDeleteScheduler(d *db.DB, fingerprints intents.FingerprintSource, origin intents.Origin, reason string) *intentDeferredDeleteScheduler {
	return &intentDeferredDeleteScheduler{db: d, store: intents.NewStore(d).WithFingerprints(fingerprints), origin: origin, reason: reason}
}

// WithTx rebinds the scheduler onto the caller's transaction: the intent
// enqueue and the caller's subscription update commit or roll back together.
func (s *intentDeferredDeleteScheduler) WithTx(tx pgx.Tx) subscriptions.DeferredDeleteScheduler {
	if s == nil {
		return s
	}
	txdb := s.db.NewWithPgxTx(tx)
	return &intentDeferredDeleteScheduler{
		db:     txdb,
		store:  intents.NewStore(txdb).WithFingerprints(s.store.Fingerprints),
		origin: s.origin,
		reason: s.reason,
	}
}

// ScheduleNMIDelete enqueues the deferred delete intent, due at runAt.
// Idempotent per subscription (intents idempotency_key): repeated cancels of
// the same subscription refresh the pending intent instead of stacking
// duplicates; a re-cancel after a resume revives the superseded intent.
func (s *intentDeferredDeleteScheduler) ScheduleNMIDelete(ctx context.Context, userID string, subscriptionID uuid.UUID, runAt time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("intent ledger unavailable for deferred delete scheduling")
	}
	// The subscription row carries the tenant + provider the intent needs.
	sub, err := s.db.Gen(ctx).GetSubscriptionByID(ctx, subscriptionID)
	if err != nil {
		return fmt.Errorf("load subscription for deferred delete intent: %w", err)
	}
	_, err = s.store.Enqueue(ctx, intents.EnqueueParams{
		MerchantID:     sub.MerchantID,
		Provider:       strings.ToLower(sub.Processor),
		IntentType:     intents.TypeNMIDeleteSubscription,
		SubscriptionID: &subscriptionID,
		Payload: intents.NMIDeletePayload{
			UserID:                  userID,
			ProcessorSubscriptionID: sub.ProcessorSubscriptionID,
		},
		IdempotencyKey: intents.NMIDeleteIdempotencyKey(subscriptionID),
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
func (s *intentDeferredDeleteScheduler) CancelNMIDelete(ctx context.Context, userID string, subscriptionID uuid.UUID) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.store.SupersedeBySubject(ctx, intents.TypeNMIDeleteSubscription, subscriptionID,
		"cancellation undone (resume) for user "+userID)
	return err
}

// ConvertDeferredDeleteMarkersToIntents is the startup sweep that converts
// any cancelled subscription still carrying a DeletionScheduledAt marker
// WITHOUT a live intent into a durable nmi_delete_subscription intent (the
// marker stays, as the read model, until the intent's finalize clears it).
//
// In steady state this finds nothing: the producers write marker + intent in
// ONE transaction, so they cannot diverge. The sweep exists for markers
// written OUT OF BAND — above all direct-DB imports (the doujins legacy
// migration stamps deletion_scheduled_at on imported void subscriptions,
// #391, and this sweep is what turns them into delete intents on the next
// boot). Idempotent via the intent idempotency_key. Each intent is due at
// max(now, DeletionScheduledAt): a still-open undo window is honored,
// anything overdue runs on the executor's next pass.
//
// Converted markers are enqueued user-origin: the pre-ledger delete job
// executed under mode=limited (only the kill switch gated it), and most
// markers are user cancellations' undo windows — system-origin would park
// them under limited and silently change behavior.
func (r *Runtime) ConvertDeferredDeleteMarkersToIntents(ctx context.Context) (int, error) {
	if r == nil || r.DB == nil {
		return 0, nil
	}

	rows, err := r.DB.Gen(ctx).ListPendingDeletionScheduledSubscriptions(ctx)
	if err != nil {
		return 0, fmt.Errorf("list pending deferred-delete markers: %w", err)
	}
	if len(rows) == 0 {
		log.Info("Deferred-delete marker sweep: no pending markers")
		return 0, nil
	}

	now := time.Now().UTC()
	if r.Clock != nil {
		now = r.Clock.Now().UTC()
	}
	store := intents.NewStore(r.DB).WithFingerprints(r.AccountFingerprints())

	converted := 0
	for _, sub := range rows {
		if !processors.IsNMIBackedProcessor(models.Processor(sub.Processor)) {
			// Only NMI-backed paths set the marker; skip defensively (the
			// marker stays discoverable for #107 reconciliation).
			log.WithFields(log.Fields{
				"subscription_id": sub.ID,
				"processor":       sub.Processor,
			}).Warn("Deferred-delete marker sweep: skipping non-NMI-backed subscription with deletion marker")
			continue
		}
		runAt := now
		if sub.DeletionScheduledAt != nil && sub.DeletionScheduledAt.After(now) {
			runAt = sub.DeletionScheduledAt.UTC()
		}
		subID := sub.ID
		_, err := store.Enqueue(ctx, intents.EnqueueParams{
			MerchantID:     sub.MerchantID,
			Provider:       strings.ToLower(sub.Processor),
			IntentType:     intents.TypeNMIDeleteSubscription,
			SubscriptionID: &subID,
			Payload: intents.NMIDeletePayload{
				UserID:                  sub.CustomerID.String(),
				ProcessorSubscriptionID: sub.ProcessorSubscriptionID,
			},
			IdempotencyKey: intents.NMIDeleteIdempotencyKey(subID),
			NextAttemptAt:  runAt,
			Origin:         intents.OriginUser,
			OriginReason:   "deferred-delete marker conversion (startup sweep)",
		})
		if err != nil {
			log.WithError(err).WithFields(log.Fields{
				"subscription_id": sub.ID,
			}).Error("Deferred-delete marker sweep: failed to enqueue intent")
			continue
		}
		converted++
	}

	log.WithFields(log.Fields{
		"pending":   len(rows),
		"converted": converted,
	}).Info("Deferred-delete marker sweep completed")
	return converted, nil
}
