//go:build integration

package subscriptions

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/stretchr/testify/require"
)

type countingSubscriptionCreditGranter struct {
	calls int
}

func (g *countingSubscriptionCreditGranter) GrantSubscriptionCreditsTx(context.Context, *gen.Queries, SubscriptionCreditGrantParams) error {
	g.calls++
	return nil
}

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

func TestCreateMembership_ReplaysRecordedPaymentForBillableSubscription(t *testing.T) {
	for _, status := range []models.SubscriptionStatus{models.StatusActive, models.StatusPastDue} {
		t.Run(string(status), func(t *testing.T) {
			f := newFailopenFixture(t, 24*30, true)
			ctx := failopenCtx()
			txnID := "txn_billable_replay_" + uuid.NewString()
			subID := seedPendingSubscriptionWithPayment(t, f, "sub_billable_replay_"+uuid.NewString(), txnID)
			periodStart := time.Now().UTC()
			periodEnd := periodStart.Add(30 * 24 * time.Hour)
			_, err := f.pool.Exec(ctx, `UPDATE openrails.subscriptions
				SET status=$2, current_period_starts_at=$3, current_period_ends_at=$4
				WHERE id=$1`, subID, status, periodStart, periodEnd)
			require.NoError(t, err)

			sub, err := f.lifecycle.CreateMembership(ctx, &CreateMembershipParams{
				UserID:        f.userID,
				PriceID:       f.priceID,
				Rail:          models.RailNMI,
				TransactionID: txnID,
			})
			require.NoError(t, err)
			require.Equal(t, subID, sub.ID)
			require.Equal(t, status, sub.Status)
		})
	}
}

func TestCreateMembership_RejectsRecordedPaymentForNonBillableSubscription(t *testing.T) {
	tests := []struct {
		name   string
		status models.SubscriptionStatus
	}{
		{name: "cancelled after provider expiry", status: models.StatusCancelled},
		{name: "unknown pending provider verification", status: models.StatusUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFailopenFixture(t, 24*30, true)
			ctx := failopenCtx()
			txnID := "txn_non_billable_replay_" + uuid.NewString()
			subID := seedPendingSubscriptionWithPayment(t, f, "sub_non_billable_replay_"+uuid.NewString(), txnID)
			periodStart := time.Now().UTC()
			periodEnd := periodStart.Add(30 * 24 * time.Hour)
			if tt.status == models.StatusCancelled {
				cancelType := models.CancelTypeExpired
				_, err := f.pool.Exec(ctx, `UPDATE openrails.subscriptions
					SET status=$2, current_period_starts_at=$3, current_period_ends_at=$4,
					    cancelled_at=$3, cancel_type=$5,
					    credits_spec_snapshot='{"welcome":{"unit":"USD","amount":25,"cadence":"once"}}'::jsonb
					WHERE id=$1`, subID, tt.status, periodStart, periodEnd, cancelType)
				require.NoError(t, err)
			} else {
				_, err := f.pool.Exec(ctx, `UPDATE openrails.subscriptions
					SET status=$2, current_period_starts_at=$3, current_period_ends_at=$4,
					    credits_spec_snapshot='{"welcome":{"unit":"USD","amount":25,"cadence":"once"}}'::jsonb
					WHERE id=$1`, subID, tt.status, periodStart, periodEnd)
				require.NoError(t, err)
			}

			credits := &countingSubscriptionCreditGranter{}
			f.lifecycle.SetCreditGranter(credits)
			sub, err := f.lifecycle.CreateMembership(ctx, &CreateMembershipParams{
				UserID:        f.userID,
				PriceID:       f.priceID,
				Rail:          models.RailNMI,
				TransactionID: txnID,
			})
			require.Nil(t, sub)
			require.ErrorContains(t, err, "status \""+string(tt.status)+"\"")
			require.Zero(t, credits.calls, "a rejected replay must not grant subscription credits")
			require.Equal(t, tt.status, f.loadSub(t, subID).Status)
		})
	}
}
