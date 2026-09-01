package riverjobs

import (
	"context"
	"fmt"

	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/progress"
	"github.com/open-rails/openrails/pkg/merchant"
)

const KindNotificationEmailSweep = "openrails.notification_email_sweep"

// notificationEmailSweepBatch bounds one merchant's per-sweep delivery batch;
// the remainder rides the next tick.
const notificationEmailSweepBatch = 200

type NotificationEmailSweepArgs struct{}

func (NotificationEmailSweepArgs) Kind() string { return KindNotificationEmailSweep }

// NotificationEmailSweepWorker (#789) delivers undelivered notification_queue
// rows (emailed_at NULL) through the SAME NotificationService.DeliverEmail path
// the inline dispatch uses — DeliverEmail stamps emailed_at on success, no-ops
// (and stamps) unsupported types, and leaves failures NULL for the next sweep.
// It is the delivery half of the converge NOTIFY pass, which only creates rows.
// Mirrors ConvergeSweepWorker: privileged merchant list, per-merchant
// RunInMerchantConn, one merchant's failure never aborts the rest.
type NotificationEmailSweepWorker struct {
	river.WorkerDefaults[NotificationEmailSweepArgs]
	DB            *db.DB
	Notifications *subscriptions.NotificationService
}

func (NotificationEmailSweepWorker) Kind() string { return KindNotificationEmailSweep }

func (w NotificationEmailSweepWorker) Work(ctx context.Context, job *river.Job[NotificationEmailSweepArgs]) error {
	logger := log.WithContext(ctx).WithField("worker", KindNotificationEmailSweep)
	if w.Notifications == nil || !w.Notifications.EmailEnabled() {
		logger.Debug("email service not armed - leaving notifications undelivered")
		return nil
	}

	// openrails.merchants is the policy-free directory (one of the four
	// RLS-exempt tables), so this read genuinely works on the base pool. It is
	// NOT a privileged read — no such thing exists — and every merchant-owned
	// query below runs inside RunInMerchantConn (or#868).
	merchantIDs, err := w.DB.GenDirectory().ListActiveMerchantIDs(ctx)
	if err != nil {
		return fmt.Errorf("notification email sweep: list merchants: %w", err)
	}

	var delivered, failed int
	for _, mid := range merchantIDs {
		progress.Mark(ctx, "notification email merchant "+mid.String())
		mctx := merchant.WithID(ctx, merchant.ID(mid))
		if err := w.DB.RunInMerchantConn(mctx, func(ctx context.Context) error {
			rows, err := w.DB.Gen(ctx).ListUndeliveredNotifications(ctx, gen.ListUndeliveredNotificationsParams{
				MerchantID: mid, PageLimit: notificationEmailSweepBatch,
			})
			if err != nil {
				return fmt.Errorf("list undelivered notifications: %w", err)
			}
			for i := range rows {
				n, err := models.NotificationFromGen(rows[i])
				if err != nil {
					failed++
					logger.WithError(err).WithField("notification_id", rows[i].ID).
						Error("notification email sweep: decode row; skipping")
					continue
				}
				if err := w.Notifications.DeliverEmail(ctx, n); err != nil {
					failed++
					logger.WithError(err).WithFields(log.Fields{
						"notification_id": n.ID, "event_type": n.EventType,
					}).Error("notification email sweep: delivery failed; will retry next sweep")
					continue
				}
				delivered++
			}
			return nil
		}); err != nil {
			// One merchant's failure must not abort the rest of the sweep.
			logger.WithError(err).WithField("merchant_id", mid).
				Error("notification email sweep: merchant failed; continuing")
		}
	}
	if delivered > 0 || failed > 0 {
		logger.WithFields(log.Fields{"delivered": delivered, "failed": failed}).
			Info("notification email sweep completed")
	}
	return nil
}
