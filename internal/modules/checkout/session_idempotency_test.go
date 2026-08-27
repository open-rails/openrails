package checkout

import (
	"context"
	"encoding/json"
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

func TestCCBillFingerprintUsesExecutedEmailProjection(t *testing.T) {
	requestA := &CheckoutSessionCreateRequest{
		PriceID: "price_11111111-1111-1111-1111-111111111111",
		Payment: CheckoutSessionPaymentRequest{
			Rail:       "merchant-ccbill",
			Email:      "browser-a@example.test",
			NameOnCard: "Buyer Example",
			Zip:        "10001",
			Country:    "US",
		},
	}
	requestB := *requestA
	requestB.Payment.Email = "browser-b@example.test"
	verifiedEmail := "verified@example.test"
	user := &UserIdentity{ID: "user_123", Email: &verifiedEmail}

	require.Equal(t,
		checkoutSessionRequestFingerprintForRail(requestA, user, "ccbill"),
		checkoutSessionRequestFingerprintForRail(&requestB, user, "ccbill"),
		"ignored browser email must not create an idempotency conflict",
	)
	require.NotEqual(t,
		checkoutSessionRequestFingerprintForRail(requestA, user, "stripe"),
		checkoutSessionRequestFingerprintForRail(&requestB, user, "stripe"),
		"other rails retain their browser request projection",
	)
}

func TestCCBillFingerprintBindsAuthoritativeVerifiedEmail(t *testing.T) {
	req := &CheckoutSessionCreateRequest{
		PriceID: "price_11111111-1111-1111-1111-111111111111",
		Payment: CheckoutSessionPaymentRequest{
			Rail:       "merchant-ccbill",
			Email:      "browser@example.test",
			NameOnCard: "Buyer Example",
			Zip:        "10001",
			Country:    "US",
		},
	}
	firstEmail := "first-verified@example.test"
	secondEmail := "second-verified@example.test"

	require.NotEqual(t,
		checkoutSessionRequestFingerprintForRail(req, &UserIdentity{ID: "user_123", Email: &firstEmail}, "ccbill"),
		checkoutSessionRequestFingerprintForRail(req, &UserIdentity{ID: "user_123", Email: &secondEmail}, "ccbill"),
		"a changed authoritative identity must conflict rather than replay another identity's form",
	)
}

func TestDecodeCCBillIdempotencyUsesAuthoritativeEmailProjection(t *testing.T) {
	req := &CheckoutSessionCreateRequest{
		PriceID: "price_11111111-1111-1111-1111-111111111111",
		Payment: CheckoutSessionPaymentRequest{
			Rail:       "merchant-ccbill",
			Email:      "first-browser@example.test",
			NameOnCard: "Buyer Example",
			Zip:        "10001",
			Country:    "US",
		},
	}
	verifiedEmail := "verified@example.test"
	user := &UserIdentity{ID: "user_123", Email: &verifiedEmail}
	response := &CheckoutSessionResponse{Payment: CheckoutSessionPaymentResponse{Rail: "ccbill"}}
	payload, err := json.Marshal(checkoutSessionIdempotencyResult{
		RequestFingerprint: checkoutSessionRequestFingerprintForRail(req, user, "ccbill"),
		Response:           response,
	})
	require.NoError(t, err)

	retry := *req
	retry.Payment.Email = "different-browser@example.test"
	got, err := decodeCheckoutSessionIdempotencyResult(payload, &retry, user)
	require.NoError(t, err)
	require.Equal(t, response, got)

	changedEmail := "changed-verified@example.test"
	_, err = decodeCheckoutSessionIdempotencyResult(payload, &retry, &UserIdentity{ID: "user_123", Email: &changedEmail})
	require.ErrorIs(t, err, ErrCheckoutSessionConflict)
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

func TestValidateCCBillInputAcceptsMinimalBillingIdentity(t *testing.T) {
	verifiedEmail := "prince@example.test"
	payment := &CheckoutSessionPaymentRequest{
		Email:      "spoofed-browser@example.test",
		NameOnCard: "Prince",
		Zip:        "55401",
		Country:    "US",
	}
	svc := &CheckoutSessionService{}
	user := &UserIdentity{ID: "user_123", Email: &verifiedEmail}

	require.NoError(t, svc.validateCCBillInput(payment, user))
	fields := svc.buildRailFields("ccbill", payment, user)
	require.Equal(t, verifiedEmail, fields["email"])
	require.NotContains(t, fields, "address1")
	require.NotContains(t, fields, "city")
	require.NotContains(t, fields, "state")
}

func TestValidateCCBillInputRequiresCanonicalNameVerifiedEmailCountryAndPostal(t *testing.T) {
	verifiedEmail := "buyer@example.test"
	validPayment := CheckoutSessionPaymentRequest{NameOnCard: "Buyer Example", Zip: "10001", Country: "US"}
	validUser := &UserIdentity{ID: "user_123", Email: &verifiedEmail}

	tests := []struct {
		name    string
		payment CheckoutSessionPaymentRequest
		user    *UserIdentity
		want    string
	}{
		{name: "name", payment: CheckoutSessionPaymentRequest{Zip: "10001", Country: "US"}, user: validUser, want: "name_on_card"},
		{name: "postal", payment: CheckoutSessionPaymentRequest{NameOnCard: "Buyer Example", Country: "US"}, user: validUser, want: "zip"},
		{name: "country", payment: CheckoutSessionPaymentRequest{NameOnCard: "Buyer Example", Zip: "10001"}, user: validUser, want: "country"},
		{name: "non-letter country", payment: CheckoutSessionPaymentRequest{NameOnCard: "Buyer Example", Zip: "10001", Country: "12"}, user: validUser, want: "country"},
		{name: "verified email", payment: validPayment, user: &UserIdentity{ID: "user_123"}, want: "verified email"},
	}

	svc := &CheckoutSessionService{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := svc.validateCCBillInput(&tt.payment, tt.user)
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestValidateCCBillInputCanonicalizesCountryAndPostal(t *testing.T) {
	verifiedEmail := "buyer@example.test"
	payment := &CheckoutSessionPaymentRequest{NameOnCard: "Buyer Example", Zip: " 10001 ", Country: " us "}

	require.NoError(t, (&CheckoutSessionService{}).validateCCBillInput(
		payment,
		&UserIdentity{ID: "user_123", Email: &verifiedEmail},
	))
	require.Equal(t, "10001", payment.Zip)
	require.Equal(t, "US", payment.Country)
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
