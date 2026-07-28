package handlers

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/stretchr/testify/require"
)

func TestPaymentToAPIStatusAndCaptured(t *testing.T) {
	tests := []struct {
		name         string
		status       string
		wantStatus   string
		wantCaptured bool
	}{
		{name: "completed", status: "completed", wantStatus: "succeeded", wantCaptured: true},
		{name: "empty default", status: "", wantStatus: "succeeded", wantCaptured: true},
		{name: "pending", status: "pending", wantStatus: "pending", wantCaptured: false},
		{name: "failed", status: "failed", wantStatus: "failed", wantCaptured: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payment := &models.Payment{
				ID:            uuid.New(),
				CustomerID:    identity.CustomerIDFromString(uuid.NewString()).UUID(),
				PriceID:       uuid.New(),
				Rail:          models.RailNMI,
				TransactionID: "txn_123",
				Amount:        1000,
				Currency:      "USD",
				Status:        tt.status,
				CreatedAt:     time.Unix(100, 0),
			}

			got := PaymentToAPI(payment, nil)
			require.Equal(t, tt.wantStatus, got.Status)
			require.Equal(t, tt.wantCaptured, got.Captured)
		})
	}
}

func TestPaymentToAPIRefundObjectNotCaptured(t *testing.T) {
	originalID := uuid.New()
	refund := &models.Payment{
		ID:                uuid.New(),
		CustomerID:        identity.CustomerIDFromString(uuid.NewString()).UUID(),
		PriceID:           uuid.New(),
		RefundedPaymentID: &originalID,
		Rail:              models.RailNMI,
		TransactionID:     "refund_123",
		Amount:            -500,
		Currency:          "USD",
	}

	got := PaymentToAPI(refund, nil)
	require.Equal(t, "refund", got.Object)
	require.Equal(t, "succeeded", got.Status)
	require.False(t, got.Captured)
}

func TestPaymentToAPIRefundObjectPreservesPendingAndFailedStatus(t *testing.T) {
	originalID := uuid.New()
	for _, tt := range []struct {
		status string
		want   string
	}{
		{status: "pending", want: "pending"},
		{status: "failed", want: "failed"},
	} {
		t.Run(tt.status, func(t *testing.T) {
			refund := &models.Payment{
				ID:                uuid.New(),
				CustomerID:        identity.CustomerIDFromString(uuid.NewString()).UUID(),
				PriceID:           uuid.New(),
				RefundedPaymentID: &originalID,
				Rail:              models.RailNMI,
				TransactionID:     "refund_123",
				Amount:            -500,
				Currency:          "USD",
				Status:            tt.status,
			}

			got := PaymentToAPI(refund, nil)
			require.Equal(t, "refund", got.Object)
			require.Equal(t, tt.want, got.Status)
			require.False(t, got.Captured)
		})
	}
}

func TestPaymentToAPIAmountRefundedCountsOnlyCompletedRefunds(t *testing.T) {
	originalID := uuid.New()
	payment := &models.Payment{
		ID:            originalID,
		CustomerID:    identity.CustomerIDFromString(uuid.NewString()).UUID(),
		PriceID:       uuid.New(),
		Rail:          models.RailNMI,
		TransactionID: "txn_123",
		Amount:        1000,
		Currency:      "USD",
		Status:        "completed",
		CreatedAt:     time.Unix(100, 0),
	}
	refunds := []*models.Payment{
		{ID: uuid.New(), CustomerID: payment.CustomerID, PriceID: payment.PriceID, RefundedPaymentID: &originalID, Rail: payment.Rail, TransactionID: "refund_completed", Amount: -300, Currency: "USD", Status: "completed"},
		{ID: uuid.New(), CustomerID: payment.CustomerID, PriceID: payment.PriceID, RefundedPaymentID: &originalID, Rail: payment.Rail, TransactionID: "refund_pending", Amount: -400, Currency: "USD", Status: "pending"},
		{ID: uuid.New(), CustomerID: payment.CustomerID, PriceID: payment.PriceID, RefundedPaymentID: &originalID, Rail: payment.Rail, TransactionID: "refund_failed", Amount: -500, Currency: "USD", Status: "failed"},
	}

	got := PaymentToAPI(payment, refunds)
	require.Equal(t, int64(300), got.AmountRefunded)
	require.Equal(t, "partially_refunded", got.Status)
	require.NotNil(t, got.Refunds)
	require.Len(t, got.Refunds.Data, 3)
	require.Equal(t, "pending", got.Refunds.Data[1].Status)
	require.Equal(t, "failed", got.Refunds.Data[2].Status)
}

func TestProductToAPIPreservesCreditGrantUnit(t *testing.T) {
	product := &models.Product{
		ID:          uuid.New(),
		DisplayName: "Premium",
		CreditsSpec: models.CreditsSpec{
			"monthly_eur": {Unit: "EUR", Amount: 2500},
		},
	}

	got := ProductToAPI(product, nil)
	require.Equal(t, "EUR", got.CreditsSpec["monthly_eur"].Unit)
	require.Equal(t, int64(2500), got.CreditsSpec["monthly_eur"].Amount)
}
