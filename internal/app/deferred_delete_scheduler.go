package app

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/modules/payments/processors"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	log "github.com/sirupsen/logrus"
)

// riverDeferredDeleteScheduler implements subscriptions.DeferredDeleteScheduler
// on top of the River producer. It schedules and cancels the deferred NMI
// delete_subscription job (issue 216).
type riverDeferredDeleteScheduler struct {
	producer *river.Client[pgx.Tx]
}

func newRiverDeferredDeleteScheduler(producer *river.Client[pgx.Tx]) *riverDeferredDeleteScheduler {
	return &riverDeferredDeleteScheduler{producer: producer}
}

// ScheduleNMIDelete enqueues the deferred delete to run at runAt. Unique on
// (user_id, subscription_id) so repeated cancels of the same subscription do not
// stack duplicate deletes.
func (s *riverDeferredDeleteScheduler) ScheduleNMIDelete(ctx context.Context, userID string, subscriptionID uuid.UUID, runAt time.Time) error {
	if s == nil || s.producer == nil {
		return fmt.Errorf("river producer unavailable for deferred delete scheduling")
	}
	_, err := s.producer.Insert(ctx, riverjobs.NMIDeleteSubscriptionArgs{
		UserID:         userID,
		SubscriptionID: subscriptionID,
	}, &river.InsertOpts{
		Queue:       riverjobs.QueueBilling,
		ScheduledAt: runAt.UTC(),
		UniqueOpts: river.UniqueOpts{
			ByArgs:  true,
			ByQueue: true,
			ByState: []rivertype.JobState{
				rivertype.JobStateAvailable,
				rivertype.JobStatePending,
				rivertype.JobStateRunning,
				rivertype.JobStateRetryable,
				rivertype.JobStateScheduled,
			},
		},
	})
	return err
}

// RescanPendingDeferredDeletes re-enqueues the deferred NMI delete job for
// every cancelled subscription that still carries a DeletionScheduledAt marker
// (#344 follow-up). Markers survive when the processor-subscription-deletion
// kill switch skipped a delete (or a job was lost); without this rescan nothing
// re-enqueues them after the flag is lifted. Called on worker startup, after
// the River client has started. Idempotent: ScheduleNMIDelete inserts with
// river.UniqueOpts ByArgs, so re-enqueueing an already-pending delete is a
// no-op. Each delete runs at max(now, DeletionScheduledAt) — a still-open undo
// window is honored, anything overdue runs immediately. Returns the number of
// subscriptions re-enqueued.
func (r *Runtime) RescanPendingDeferredDeletes(ctx context.Context) (int, error) {
	if r == nil || r.DB == nil {
		return 0, nil
	}
	if r.externalRiverClient || r.RiverProducer == nil {
		// External River client path: the host owns worker startup; the markers
		// stay discoverable for #107 reconciliation.
		log.Info("Skipping deferred NMI delete rescan: no in-process River producer")
		return 0, nil
	}

	subs, err := repo.NewSubscriptionRepo(r.DB).ListPendingDeletionScheduled(ctx)
	if err != nil {
		return 0, fmt.Errorf("list pending deferred deletes: %w", err)
	}
	if len(subs) == 0 {
		log.Info("Deferred NMI delete rescan: no pending markers")
		return 0, nil
	}

	now := time.Now().UTC()
	if r.Clock != nil {
		now = r.Clock.Now().UTC()
	}
	scheduler := newRiverDeferredDeleteScheduler(r.RiverProducer)

	enqueued := 0
	for _, sub := range subs {
		if !processors.IsNMIBackedProcessor(sub.Processor) {
			// Only NMI-backed paths set the marker; the delete worker would
			// finalize (clear) a marker it has no client for, so skip defensively.
			log.WithFields(log.Fields{
				"subscription_id": sub.ID,
				"processor":       sub.Processor,
			}).Warn("Deferred NMI delete rescan: skipping non-NMI-backed subscription with deletion marker")
			continue
		}
		runAt := now
		if sub.DeletionScheduledAt != nil && sub.DeletionScheduledAt.After(now) {
			runAt = sub.DeletionScheduledAt.UTC()
		}
		if err := scheduler.ScheduleNMIDelete(ctx, sub.TenantSubjectID.String(), sub.ID, runAt); err != nil {
			log.WithError(err).WithFields(log.Fields{
				"subscription_id": sub.ID,
			}).Error("Deferred NMI delete rescan: failed to re-enqueue delete job")
			continue
		}
		enqueued++
	}

	log.WithFields(log.Fields{
		"pending":    len(subs),
		"reenqueued": enqueued,
	}).Info("Deferred NMI delete rescan completed")
	return enqueued, nil
}

// CancelNMIDelete best-effort cancels any pending deferred-delete job for the
// subscription. Advisory only: the delete worker re-checks state and no-ops if
// the cancellation was resumed.
func (s *riverDeferredDeleteScheduler) CancelNMIDelete(ctx context.Context, userID string, subscriptionID uuid.UUID) error {
	if s == nil || s.producer == nil {
		return nil
	}
	jobs, err := s.producer.JobList(ctx, river.NewJobListParams().
		Kinds(riverjobs.KindSubscriptionNMIDelete).
		States(rivertype.JobStateScheduled, rivertype.JobStateAvailable, rivertype.JobStateRetryable, rivertype.JobStatePending).
		First(1000))
	if err != nil {
		return err
	}
	for _, j := range jobs.Jobs {
		var args riverjobs.NMIDeleteSubscriptionArgs
		if err := json.Unmarshal(j.EncodedArgs, &args); err != nil {
			continue
		}
		if args.SubscriptionID == subscriptionID && args.UserID == userID {
			if _, err := s.producer.JobCancel(ctx, j.ID); err != nil {
				log.WithContext(ctx).WithError(err).WithField("job_id", j.ID).Warn("cancel deferred NMI delete job failed")
			}
		}
	}
	return nil
}
