//go:build integration

package riverjobs

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// #789: with no armed email service the sweep must leave undelivered rows
// exactly as they are (emailed_at NULL) without erroring — they are retried
// once email is wired, never stamped as sent.
func TestNotificationEmailSweep_NoEmailServiceLeavesRowsUndelivered(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	merchantID := dbtest.TestMerchantID.UUID()
	baseCtx := dbtest.WithTestMerchant(context.Background())
	dbtest.EnsureTestMerchant(baseCtx, t, dbi.Pool())

	notifID := uuid.New()
	var customer uuid.UUID
	require.NoError(t, dbi.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		customer = dbtest.EnsureCustomerIDPgx(ctx, t, dbi.Qx(ctx), uuid.NewString())
		_, err := dbi.Qx(ctx).Exec(ctx,
			`INSERT INTO openrails.notification_queue (id, merchant_id, customer_id, event_type, data)
			 VALUES ($1,$2,$3,'premium_ended','{"reason":"access_ended","source":"converge_notify"}'::jsonb)`,
			notifID, merchantID, customer)
		return err
	}))
	t.Cleanup(func() {
		_ = dbi.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = dbi.Qx(ctx).Exec(ctx, `DELETE FROM openrails.notification_queue WHERE id=$1`, notifID)
			return nil
		})
	})

	worker := NotificationEmailSweepWorker{
		DB:            dbi,
		Notifications: subscriptions.NewNotificationService(dbi, nil), // no email service
	}
	require.NoError(t, worker.Work(context.Background(), &river.Job[NotificationEmailSweepArgs]{}))

	require.NoError(t, dbi.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		var emailedAt *time.Time
		require.NoError(t, dbi.Qx(ctx).QueryRow(ctx,
			`SELECT emailed_at FROM openrails.notification_queue WHERE id=$1`, notifID).Scan(&emailedAt))
		require.Nil(t, emailedAt, "no email service ⇒ row stays undelivered for a later sweep")
		return nil
	}))
}
