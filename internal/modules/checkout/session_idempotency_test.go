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

func TestCheckoutSessionRequestFingerprintIncludesRedirectURLs(t *testing.T) {
	t.Parallel()

	base := CheckoutSessionCreateRequest{
		PriceID:    "price_11111111-1111-1111-1111-111111111111",
		Payment:    CheckoutSessionPaymentRequest{Rail: "stripe"},
		SuccessURL: "https://app.example.test/billing?checkout=success",
		CancelURL:  "https://app.example.test/billing?checkout=cancelled",
	}

	tests := []struct {
		name   string
		mutate func(*CheckoutSessionCreateRequest)
	}{
		{
			name: "success url changes",
			mutate: func(req *CheckoutSessionCreateRequest) {
				req.SuccessURL = "https://other.example.test/billing?checkout=success"
			},
		},
		{
			name: "cancel url changes",
			mutate: func(req *CheckoutSessionCreateRequest) {
				req.CancelURL = "https://other.example.test/billing?checkout=cancelled"
			},
		},
		{
			name: "name on card changes",
			mutate: func(req *CheckoutSessionCreateRequest) {
				req.Payment.NameOnCard = "María José Carreño Quiñones"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			changed := base
			tt.mutate(&changed)
			require.NotEqual(t, checkoutSessionRequestFingerprint(&base), checkoutSessionRequestFingerprint(&changed))
		})
	}

	trimmed := base
	trimmed.SuccessURL = "  " + base.SuccessURL + "  "
	trimmed.CancelURL = "  " + base.CancelURL + "  "
	require.Equal(t, checkoutSessionRequestFingerprint(&base), checkoutSessionRequestFingerprint(&trimmed))
}

func TestCanonicalizeCheckoutPaymentName(t *testing.T) {
	canonical := CheckoutSessionPaymentRequest{
		NameOnCard: "  李  小龍  ",
		FirstName:  "ignored",
		LastName:   "legacy",
	}
	canonicalizeCheckoutPaymentName(&canonical)
	require.Equal(t, "李  小龍", canonical.NameOnCard, "canonical full value preserves internal spacing")
	require.Equal(t, "李", canonical.FirstName)
	require.Equal(t, "小龍", canonical.LastName)

	legacy := CheckoutSessionPaymentRequest{FirstName: "María de", LastName: "la Vega"}
	canonicalizeCheckoutPaymentName(&legacy)
	require.Equal(t, "María de la Vega", legacy.NameOnCard)
	require.Equal(t, "María de", legacy.FirstName, "legacy explicit split is preserved")
	require.Equal(t, "la Vega", legacy.LastName)
}

func TestRequireBillingFieldsAcceptsCanonicalMononym(t *testing.T) {
	payment := &CheckoutSessionPaymentRequest{
		Email:      "prince@example.test",
		NameOnCard: "Prince",
		Address1:   "1 Main St",
		City:       "Minneapolis",
		Zip:        "55401",
		Country:    "US",
	}
	require.NoError(t, requireBillingFields(payment))
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
