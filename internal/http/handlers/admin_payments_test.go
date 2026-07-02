package handlers

import (
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

func TestAdminRefundReservationTransactionIDIsStableAndScoped(t *testing.T) {
	paymentID := uuid.New()

	first := adminRefundReservationTransactionID(paymentID, "refund-key")
	second := adminRefundReservationTransactionID(paymentID, " refund-key ")
	otherPayment := adminRefundReservationTransactionID(uuid.New(), "refund-key")
	otherKey := adminRefundReservationTransactionID(paymentID, "other-key")

	require.Equal(t, first, second)
	require.NotEqual(t, first, otherPayment)
	require.NotEqual(t, first, otherKey)
	require.Contains(t, first, paymentID.String())
}

func TestAdminRefundMetadataIncludesIdempotencyAndProviderRefund(t *testing.T) {
	metadata := adminRefundMetadata("key-123", refundRequest{Amount: 500, Reason: "requested_by_customer"}, "completed", "re_123")

	require.Equal(t, "key-123", metadata["admin_refund_idempotency_key"])
	require.Equal(t, "completed", metadata["admin_refund_status"])
	require.Equal(t, int64(500), metadata["admin_refund_amount"])
	require.Equal(t, "requested_by_customer", metadata["admin_refund_reason"])
	require.Equal(t, "re_123", metadata["provider_refund_id"])
}

func TestAdminRefundMatchesRequest(t *testing.T) {
	existing := &models.Payment{
		Amount: -500,
		Metadata: map[string]any{
			"admin_refund_reason": "requested_by_customer",
		},
	}

	require.True(t, adminRefundMatchesRequest(existing, refundRequest{Amount: 500, Reason: " requested_by_customer "}))
	require.False(t, adminRefundMatchesRequest(existing, refundRequest{Amount: 400, Reason: "requested_by_customer"}))
	require.False(t, adminRefundMatchesRequest(existing, refundRequest{Amount: 500, Reason: "duplicate"}))
}

// TestRefundAmountCents pins the #671 micros->cents refund boundary: req.Amount
// (MICROS) becomes RefundPayload.AmountCents / the NMI-Stripe wire amount in
// CENTS. Refunds must be exact — 5_000 micros ($0.005) is an ERROR, never a
// rounded $50 or $0.01.
func TestRefundAmountCents(t *testing.T) {
	tests := []struct {
		name      string
		micros    int64
		wantCents int64
		wantErr   bool
	}{
		{name: "sixty dollars", micros: 60_000_000, wantCents: 6000},
		{name: "one cent", micros: 10_000, wantCents: 1},
		{name: "nineteen ninety nine", micros: 19_990_000, wantCents: 1999},
		{name: "sub-cent errors", micros: 5_000, wantErr: true},
		{name: "off-by-one micro errors", micros: 60_000_001, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cents, err := refundAmountCents(tt.micros)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantCents, cents)
		})
	}
}
