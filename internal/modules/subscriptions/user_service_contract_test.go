package subscriptions

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/stretchr/testify/require"
)

func TestUserSubscriptionResponsePaymentMethodContract(t *testing.T) {
	t.Parallel()

	paymentMethodID := uuid.New()
	pspID := uuid.New()
	subscription := &models.Subscription{
		ID:                 uuid.New(),
		CustomerID:         uuid.New(),
		ProductID:          uuid.New(),
		PriceID:            uuid.New(),
		Status:             models.StatusActive,
		StartedAt:          time.Now().UTC(),
		Rail:               models.RailNMI,
		RailSubscriptionID: "private-subscription-ref",
		PspID:              &pspID,
		PaymentMethodID:    &paymentMethodID,
		PaymentMethod: &models.PaymentMethod{
			ID:              paymentMethodID,
			RailCustomerRef: "private-customer-ref",
			RailMethodRef:   "private-method-ref",
		},
		Metadata: json.RawMessage(`{"secret":"private-provider-secret"}`),
	}

	payload, err := json.Marshal(&UserSubscriptionResponse{Subscription: subscription})
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(payload, &got))
	require.Equal(t, api.FormatPaymentMethodID(paymentMethodID), got["payment_method_id"])
	require.NotContains(t, got, "psp_id")
	require.NotContains(t, got, "rail_customer_ref")
	require.NotContains(t, got, "rail_method_ref")
	require.NotContains(t, got, "account_id")
	require.NotContains(t, got, "rail_subscription_id")
	require.NotContains(t, got, "gateway_response")
	for _, privateValue := range []string{
		pspID.String(),
		"private-subscription-ref",
		"private-customer-ref",
		"private-method-ref",
		"private-provider-secret",
	} {
		require.False(t, strings.Contains(string(payload), privateValue))
	}
}

func TestUserSubscriptionResponseOmitsUnboundPaymentMethodContract(t *testing.T) {
	t.Parallel()

	payload, err := json.Marshal(&UserSubscriptionResponse{Subscription: &models.Subscription{
		ID:         uuid.New(),
		CustomerID: uuid.New(),
		ProductID:  uuid.New(),
		PriceID:    uuid.New(),
		Status:     models.StatusActive,
		StartedAt:  time.Now().UTC(),
		Rail:       models.RailNMI,
	}})
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(payload, &got))
	require.NotContains(t, got, "payment_method_id")
}
