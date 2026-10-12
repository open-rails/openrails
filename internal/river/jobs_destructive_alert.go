package riverjobs

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/alerting"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// DestructiveAlertArgs is a durable incident fanout or one channel delivery.
// No credentials or webhook URLs are stored in the job.
type DestructiveAlertArgs struct {
	MerchantID uuid.UUID            `json:"merchant_id"`
	HoldID     uuid.UUID            `json:"hold_id"`
	Scope      string               `json:"scope"`
	OpenedAt   time.Time            `json:"opened_at"`
	Channel    *alerting.ChannelRef `json:"channel,omitempty"`
}

func (DestructiveAlertArgs) Kind() string { return "openrails.destructive_alert" }
func DestructiveAlertOptions() *river.InsertOpts {
	return &river.InsertOpts{Queue: QueueBilling, MaxAttempts: 100, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{rivertype.JobStateAvailable, rivertype.JobStateCompleted, rivertype.JobStatePending, rivertype.JobStateRetryable, rivertype.JobStateRunning, rivertype.JobStateScheduled}}}
}

type DestructiveAlertWorker struct {
	river.WorkerDefaults[DestructiveAlertArgs]
	DB     *db.DB
	Alerts *alerting.Service
}

func (w *DestructiveAlertWorker) Work(ctx context.Context, job *river.Job[DestructiveAlertArgs]) error {
	if w.DB == nil || w.Alerts == nil {
		return fmt.Errorf("destructive alert worker is not wired")
	}
	a := job.Args
	if a.MerchantID == uuid.Nil || a.HoldID == uuid.Nil {
		return river.JobCancel(fmt.Errorf("destructive alert has no incident identity"))
	}
	ctx = merchant.WithID(ctx, billing.MerchantID(a.MerchantID))
	if a.Channel != nil {
		return w.Alerts.DeliverDestructiveAlert(ctx, a.HoldID, a.Scope, a.OpenedAt, *a.Channel)
	}
	channels, err := w.Alerts.DestructiveChannels(ctx)
	if err != nil {
		return err
	}
	return w.DB.DataPool().CommittedMerchantTx(ctx, billing.MerchantID(a.MerchantID), func(ctx context.Context, tx pgx.Tx) error {
		for _, channel := range channels {
			next := a
			next.Channel = &channel
			if err := w.DB.InsertRiverJobTx(ctx, tx, next, DestructiveAlertOptions()); err != nil {
				return err
			}
		}
		return nil
	})
}
