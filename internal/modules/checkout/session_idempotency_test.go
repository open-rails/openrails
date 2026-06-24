package checkout

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCheckoutSessionRequestFingerprintChangesWithRequest(t *testing.T) {
	base := &CheckoutSessionCreateRequest{
		PriceID: "price_11111111-1111-1111-1111-111111111111",
		Payment: CheckoutSessionPaymentRequest{Rail: "stripe", Email: "a@example.com"},
	}
	other := &CheckoutSessionCreateRequest{
		PriceID: "price_22222222-2222-2222-2222-222222222222",
		Payment: CheckoutSessionPaymentRequest{Rail: "stripe", Email: "a@example.com"},
	}

	require.NotEqual(t, checkoutSessionRequestFingerprint(base), checkoutSessionRequestFingerprint(other))
}

func TestValidatePaymentRejectsStripeSavedPaymentMethod(t *testing.T) {
	svc := &CheckoutSessionService{}
	err := svc.validatePayment(context.Background(), "stripe", &CheckoutSessionPaymentRequest{
		PaymentMethodID: "pm_11111111-1111-1111-1111-111111111111",
		Email:           "a@example.com",
		FirstName:       "A",
		LastName:        "User",
		Address1:        "1 Main St",
		City:            "City",
		Zip:             "12345",
		Country:         "US",
	}, &UserIdentity{ID: "user_123"})

	require.Error(t, err)
	require.Contains(t, err.Error(), "saved payment methods are not supported")
}
