package handlers

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
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
				UserID:        uuid.NewString(),
				PriceID:       uuid.New(),
				Processor:     models.ProcessorMobius,
				TransactionID: "txn_123",
				Amount:        1000,
				Currency:      "usd",
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
		UserID:            uuid.NewString(),
		PriceID:           uuid.New(),
		RefundedPaymentID: &originalID,
		Processor:         models.ProcessorMobius,
		TransactionID:     "refund_123",
		Amount:            -500,
		Currency:          "usd",
	}

	got := PaymentToAPI(refund, nil)
	require.Equal(t, "refund", got.Object)
	require.Equal(t, "succeeded", got.Status)
	require.False(t, got.Captured)
}
