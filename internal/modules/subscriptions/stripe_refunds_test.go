package subscriptions

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStripeRefundService_CreateRefund_ValidationErrors(t *testing.T) {
	var nilSvc *StripeRefundService
	_, err := nilSvc.CreateRefund(context.Background(), RefundParams{ChargeID: "ch_123"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stripe refund service is not initialized")

	tests := []struct {
		name      string
		rails     config.RailSet
		params    RefundParams
		wantError string
	}{
		{
			name:      "nil config",
			rails:     nil,
			params:    RefundParams{ChargeID: "ch_123"},
			wantError: "stripe configuration is not available",
		},
		{
			name:      "nil stripe config",
			rails:     config.RailSet{},
			params:    RefundParams{ChargeID: "ch_123"},
			wantError: "stripe configuration is not available",
		},
		{
			name: "empty secret key",
			rails: config.RailSet{
				"stripe": {Type: config.RailTypeStripe, Stripe: &config.StripeRailConfig{SecretKey: ""}},
			},
			params:    RefundParams{ChargeID: "ch_123"},
			wantError: "stripe secret key is not configured",
		},
		{
			name: "empty charge ID",
			rails: config.RailSet{
				"stripe": {Type: config.RailTypeStripe, Stripe: &config.StripeRailConfig{SecretKey: "sk_test_123"}},
			},
			params:    RefundParams{ChargeID: ""},
			wantError: "charge_id or payment_intent_id is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &StripeRefundService{Config: &config.Config{}, Rails: tt.rails}
			_, err := svc.CreateRefund(context.Background(), tt.params)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantError)
		})
	}
}

func TestStripeRefundService_GetRefund_ValidationErrors(t *testing.T) {
	var nilSvc *StripeRefundService
	_, err := nilSvc.GetRefund(context.Background(), "re_123")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stripe refund service is not initialized")

	tests := []struct {
		name      string
		rails     config.RailSet
		refundID  string
		wantError string
	}{
		{
			name:      "nil config",
			rails:     nil,
			refundID:  "re_123",
			wantError: "stripe configuration is not available",
		},
		{
			name: "empty refund ID",
			rails: config.RailSet{
				"stripe": {Type: config.RailTypeStripe, Stripe: &config.StripeRailConfig{SecretKey: "sk_test_123"}},
			},
			refundID:  "",
			wantError: "refund_id is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &StripeRefundService{Config: &config.Config{}, Rails: tt.rails}
			_, err := svc.GetRefund(context.Background(), tt.refundID)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantError)
		})
	}
}

func TestResolveStripeRefundTarget(t *testing.T) {
	tests := []struct {
		name       string
		payment    *models.Payment
		wantTarget string
		wantErr    string
	}{
		{
			name: "prefers charge id from metadata",
			payment: &models.Payment{
				ID:            uuid.New(),
				TransactionID: "pi_should_not_win",
				Metadata: map[string]any{
					"stripe_charge_id":         "ch_123",
					"stripe_payment_intent_id": "pi_123",
				},
			},
			wantTarget: "ch_123",
		},
		{
			name: "uses payment intent from metadata",
			payment: &models.Payment{
				ID:            uuid.New(),
				TransactionID: "in_old",
				Metadata: map[string]any{
					"stripe_payment_intent_id": "pi_123",
				},
			},
			wantTarget: "pi_123",
		},
		{
			name: "falls back to transaction id when already refundable",
			payment: &models.Payment{
				ID:            uuid.New(),
				TransactionID: "ch_456",
			},
			wantTarget: "ch_456",
		},
		{
			name: "errors when no refundable stripe id is available",
			payment: &models.Payment{
				ID:            uuid.New(),
				TransactionID: "cs_old_checkout",
			},
			wantErr: "requires Stripe charge/payment_intent id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, err := ResolveStripeRefundTarget(tt.payment)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantTarget, target)
		})
	}
}
