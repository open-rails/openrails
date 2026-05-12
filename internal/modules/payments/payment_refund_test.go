package payments

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

func TestValidateRefundRejectsNonCompletedPayments(t *testing.T) {
	svc := &PaymentService{}

	tests := []struct {
		name   string
		status string
	}{
		{name: "failed", status: "failed"},
		{name: "pending", status: "pending"},
		{name: "refunded", status: "refunded"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payment := &models.Payment{
				ID:            uuid.New(),
				Amount:        1000,
				Status:        tt.status,
				TransactionID: "txn_123",
			}

			err := svc.ValidateRefund(context.Background(), payment, 500)
			require.Error(t, err)
			require.Contains(t, err.Error(), "completed")
		})
	}
}

func TestPaymentStatusCompleted(t *testing.T) {
	require.True(t, PaymentStatusCompleted(""))
	require.True(t, PaymentStatusCompleted("completed"))
	require.True(t, PaymentStatusCompleted(" Completed "))
	require.False(t, PaymentStatusCompleted("pending"))
	require.False(t, PaymentStatusCompleted("failed"))
}
