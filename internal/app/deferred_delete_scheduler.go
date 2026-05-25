package app

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
