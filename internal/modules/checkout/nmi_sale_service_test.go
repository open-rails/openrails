package checkout

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/stretchr/testify/require"
)

func TestNMISaleAttemptTransactionIDIsStable(t *testing.T) {
	require.Equal(t, "nmi_sale_attempt:sale_123", nmiSaleAttemptTransactionID(" sale_123 "))
}

func TestNMISubscriptionAttemptTransactionIDIsStableAndSynthetic(t *testing.T) {
	require.Equal(t, "nmi_sub_attempt:sub_123", nmiSubscriptionAttemptTransactionID(" sub_123 "))
	require.NotEqual(t, "txn_123", nmiSubscriptionAttemptTransactionID("sub_123"))
}

func TestNMISaleAttemptMetadata(t *testing.T) {
	metadata := nmiSaleAttemptMetadata("key-123", "order-123", map[string]string{"e2e_run_id": "run-1"}, "completed", "txn_123")

	require.Equal(t, "key-123", metadata["checkout_idempotency_key"])
	require.Equal(t, "order-123", metadata["nmi_order_id"])
	require.Equal(t, "completed", metadata["nmi_attempt_status"])
	require.Equal(t, "txn_123", metadata["provider_transaction_id"])
	require.Equal(t, "run-1", metadata["e2e_run_id"])
}

func TestNMISubscriptionAttemptMetadata(t *testing.T) {
	subscriptionID := uuid.New()
	paymentMethodID := uuid.New()
	delayedStart := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	metadata := nmiSubscriptionAttemptMetadata(
		"key-123",
		"order-123",
		"completed",
		"sub_123",
		"txn_123",
		subscriptionID,
		&paymentMethodID,
		&delayedStart,
		map[string]string{"e2e_run_id": "run-1"},
	)

	require.Equal(t, "key-123", metadata["checkout_idempotency_key"])
	require.Equal(t, "order-123", metadata["nmi_subscription_order_id"])
	require.Equal(t, "completed", metadata["nmi_attempt_status"])
	require.Equal(t, "sub_123", metadata["provider_subscription_id"])
	require.Equal(t, "txn_123", metadata["provider_transaction_id"])
	require.Equal(t, subscriptionID.String(), metadata["local_subscription_id"])
	require.Equal(t, paymentMethodID.String(), metadata["payment_method_id"])
	require.Equal(t, delayedStart.Format(time.RFC3339), metadata["delayed_start"])
	require.Equal(t, "run-1", metadata["e2e_run_id"])
}

func TestNMISubscriptionAttemptStatusPrefersMetadata(t *testing.T) {
	attempt := &models.Payment{
		Status: payments.PaymentStatusPendingValue,
		Metadata: map[string]any{
			"nmi_attempt_status": payments.PaymentStatusCompletedValue,
		},
	}

	require.Equal(t, payments.PaymentStatusCompletedValue, nmiSubscriptionAttemptStatusFromPayment(attempt))
}

func TestNMISubscriptionAttemptStatusFallsBackToPaymentStatus(t *testing.T) {
	attempt := &models.Payment{Status: payments.PaymentStatusPendingValue}

	require.Equal(t, payments.PaymentStatusPendingValue, nmiSubscriptionAttemptStatusFromPayment(attempt))
}

func TestNMISubscriptionStartDate(t *testing.T) {
	now := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)
	coverageEnd := now.Add(48 * time.Hour)

	startDate, delayedStart := nmiSubscriptionStartDate(&CoverageInfo{HasCoverage: true, EndDate: &coverageEnd}, now)

	require.NotEmpty(t, startDate)
	require.NotNil(t, delayedStart)
}

func TestBuildNMIFutureStartDate(t *testing.T) {
	now := time.Date(2026, time.April, 8, 10, 30, 0, 0, time.UTC)

	tests := []struct {
		name         string
		target       time.Time
		expectedDate string
		expectedTime time.Time
	}{
		{
			name:         "same day bumps to tomorrow",
			target:       time.Date(2026, time.April, 8, 23, 59, 0, 0, time.UTC),
			expectedDate: "20260409",
			expectedTime: time.Date(2026, time.April, 9, 0, 0, 0, 0, time.UTC),
		},
		{
			name:         "past day bumps to tomorrow",
			target:       time.Date(2026, time.April, 7, 23, 59, 0, 0, time.UTC),
			expectedDate: "20260409",
			expectedTime: time.Date(2026, time.April, 9, 0, 0, 0, 0, time.UTC),
		},
		{
			name:         "future day preserved",
			target:       time.Date(2026, time.April, 12, 14, 0, 0, 0, time.UTC),
			expectedDate: "20260412",
			expectedTime: time.Date(2026, time.April, 12, 0, 0, 0, 0, time.UTC),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			formatted, startAt := buildNMIFutureStartDate(tt.target, now)
			if formatted != tt.expectedDate {
				t.Fatalf("expected %s, got %s", tt.expectedDate, formatted)
			}
			if !startAt.Equal(tt.expectedTime) {
				t.Fatalf("expected %v, got %v", tt.expectedTime, startAt)
			}
		})
	}
}
