//go:build integration

package alerting_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/require"
)

func TestUnifiedNotificationsKeepRecipientsAndReadStateIsolated(t *testing.T) {
	pool, appDB := rlsSetup(t)
	a, b := uuid.New(), uuid.New()
	seedMerchant(t, pool, a)
	seedMerchant(t, pool, b)
	payerA, payerB := uuid.New(), uuid.New()
	exec(t, pool, `INSERT INTO openrails.customers(id,merchant_id) VALUES ($1,$2),($3,$2)`, payerA, a, payerB)
	customerID, merchantID := uuid.New(), uuid.Nil
	service := newService(t, appDB, nil)
	customers := subscriptions.NewNotificationService(appDB, nil)
	inConn(t, appDB, a, func(ctx context.Context) {
		require.NoError(t, customers.Create(ctx, &models.NotificationQueue{ID: customerID, CustomerID: payerA, EventType: models.NotificationPremiumEnded, CreatedAt: time.Now()}))
		row, err := appDB.Gen(ctx).CreateMerchantNotification(ctx, gen.CreateMerchantNotificationParams{MerchantID: a, Severity: "high", Title: "Merchant only"})
		require.NoError(t, err)
		merchantID = row.ID
		bell, err := service.ListNotifications(ctx, false)
		require.NoError(t, err)
		require.Len(t, bell, 1)
		require.Equal(t, merchantID, bell[0].ID)
		mine, err := customers.GetByUserID(ctx, payerA.String())
		require.NoError(t, err)
		require.Len(t, mine, 1)
		require.Equal(t, customerID, mine[0].ID)
		others, err := customers.GetByUserID(ctx, payerB.String())
		require.NoError(t, err)
		require.Empty(t, others)
		_, err = customers.GetByID(ctx, merchantID)
		require.Error(t, err)
		require.Error(t, customers.MarkAsSeen(ctx, merchantID, payerA))
		marked, err := service.MarkNotificationRead(ctx, customerID)
		require.NoError(t, err)
		require.False(t, marked)
		require.Error(t, customers.MarkAsSeen(ctx, customerID, payerB), "another customer cannot mark this row read")
		require.NoError(t, customers.MarkAsSeen(ctx, customerID, payerA))
		count, err := service.UnreadCount(ctx)
		require.NoError(t, err)
		require.EqualValues(t, 1, count, "customer read must not mark the merchant bell read")
		mail, err := appDB.Gen(ctx).ListUndeliveredNotifications(ctx, gen.ListUndeliveredNotificationsParams{MerchantID: a, PageLimit: 10})
		require.NoError(t, err)
		require.Len(t, mail, 1)
		require.Equal(t, customerID, mail[0].ID, "merchant inbox must never enter the customer email sweep")
	})
	inConn(t, appDB, b, func(ctx context.Context) {
		bell, err := service.ListNotifications(ctx, false)
		require.NoError(t, err)
		require.Empty(t, bell)
		marked, err := service.MarkNotificationRead(ctx, merchantID)
		require.NoError(t, err)
		require.False(t, marked)
		_, err = customers.GetByID(ctx, customerID)
		require.Error(t, err)
	})
}
