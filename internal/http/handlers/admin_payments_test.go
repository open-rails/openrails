package handlers

import (
	"testing"

	"github.com/google/uuid"
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
