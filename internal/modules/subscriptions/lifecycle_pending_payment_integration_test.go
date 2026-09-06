//go:build integration

package subscriptions

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/stretchr/testify/require"
)

func seedPendingSubscriptionWithPayment(t *testing.T, f *failopenFixture, procSubID, txnID string) uuid.UUID {
	t.Helper()
	ctx := failopenCtx()
	now := time.Now().UTC()
	subID := uuid.New()
	customerID := uuid.MustParse(f.userID)
	_, err := f.pool.Exec(ctx, `INSERT INTO openrails.subscriptions
		(id, price_id, product_id, status, rail, rail_subscription_id, started_at, customer_id, merchant_id, psp_id)
		VALUES ($1, $2, $3, 'pending', 'nmi', $4, $5, $6, $7, $8)`,
		subID, f.priceID, f.productID, procSubID, now, customerID, dbtest.TestMerchantID.UUID(), failopenPSP)
	require.NoError(t, err)
	require.NoError(t, payments.NewPaymentService(f.dbi, nil).Create(ctx, &models.Payment{
		ID:             uuid.New(),
		CustomerID:     customerID,
		PriceID:        f.priceID,
		SubscriptionID: &subID,
		Rail:           models.RailNMI,
		PspID:          &failopenPSP,
		TransactionID:  txnID,
		Amount:         9990000,
		ListAmount:     9990000,
		Currency:       "USD",
		Status:         payments.PaymentStatusCompletedValue,
		MoneyMovement:  models.MoneyMovementRail,
		PurchasedAt:    now,
		CreatedAt:      now,
	}))
	return subID
}

func assertPendingActivatedOnce(t *testing.T, f *failopenFixture, subID uuid.UUID, txnID string) {
	t.Helper()
	ctx := context.Background()
	require.Len(t, f.windows(t, subID, "subscription"), 1)
	require.True(t, f.entitledAt(t, time.Now().UTC().Add(time.Minute)))
	var paymentCount int
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM openrails.payments WHERE transaction_id = $1`, txnID).Scan(&paymentCount))
	require.Equal(t, 1, paymentCount)
	rows, err := f.pool.Query(ctx, `SELECT to_status::text FROM openrails.subscription_status_transitions WHERE subscription_id = $1 ORDER BY occurred_at, id`, subID)
	require.NoError(t, err)
	defer rows.Close()
	var transitions []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		transitions = append(transitions, s)
	}
	require.Equal(t, []string{"pending", "active"}, transitions)
}

func TestCreateMembership_ActivatesPendingSubscriptionWithRecordedPayment(t *testing.T) {
	f := newFailopenFixture(t, 24*30, true)
	ctx := failopenCtx()
	procSubID := "sub_pending_paid_" + uuid.NewString()
	txnID := "txn_pending_paid_" + uuid.NewString()
	subID := seedPendingSubscriptionWithPayment(t, f, procSubID, txnID)

	for range 2 {
		sub, err := f.lifecycle.CreateMembership(ctx, &CreateMembershipParams{
			UserID:             f.userID,
			PriceID:            f.priceID,
			Rail:               models.RailNMI,
			RailSubscriptionID: &procSubID,
			TransactionID:      txnID,
			Amount:             9990000,
			AmountProvided:     true,
			Currency:           "USD",
		})
		require.NoError(t, err)
		require.Equal(t, subID, sub.ID)
		require.Equal(t, models.StatusActive, sub.Status)
	}
	assertPendingActivatedOnce(t, f, subID, txnID)
}

func TestCreateMembership_ActivatesPendingSubscriptionFoundOnlyByPayment(t *testing.T) {
	f := newFailopenFixture(t, 24*30, true)
	ctx := failopenCtx()
	txnID := "txn_pending_paid_" + uuid.NewString()
	subID := seedPendingSubscriptionWithPayment(t, f, "sub_pending_paid_"+uuid.NewString(), txnID)

	sub, err := f.lifecycle.CreateMembership(ctx, &CreateMembershipParams{
		UserID:        f.userID,
		PriceID:       f.priceID,
		Rail:          models.RailNMI,
		TransactionID: txnID,
	})
	require.NoError(t, err)
	require.Equal(t, subID, sub.ID)
	require.Equal(t, models.StatusActive, sub.Status)
	assertPendingActivatedOnce(t, f, subID, txnID)
}
