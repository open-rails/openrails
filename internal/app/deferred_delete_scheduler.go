package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// newIntentDeferredDeleteScheduler builds the intent-ledger-backed deferred
// NMI delete scheduler. The implementation moved to intents.NMIDeleteScheduler
// (#679) so the unknown-reconcile path and tests can construct the REAL
// production scheduler; this shim keeps the composition root unchanged.
func newIntentDeferredDeleteScheduler(d *db.DB, origin intents.Origin, reason string) subscriptions.DeferredDeleteScheduler {
	return intents.NewNMIDeleteScheduler(d, origin, reason)
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
	store := intents.NewStore(r.DB)

	converted := 0
	for _, sub := range rows {
		if !rails.IsNMI(models.Rail(sub.Rail)) {
			// Only NMI-backed paths set the marker; skip defensively (the
			// marker stays discoverable for #107 reconciliation).
			log.WithFields(log.Fields{
				"subscription_id": sub.ID,
				"rail":            sub.Rail,
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
			Provider:       strings.ToLower(sub.Rail),
			IntentType:     intents.TypeNMIDeleteSubscription,
			SubscriptionID: &subID,
			Payload: intents.NMIDeletePayload{
				UserID:             sub.CustomerID.String(),
				RailSubscriptionID: sub.RailSubscriptionID,
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
