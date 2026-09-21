package checkout

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
	"strconv"
	"testing"
	"time"

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

func TestInitialMembershipQuoteAndVerifiedPayerPreparation(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 123456000, time.UTC)
	mid, customer, psp, custodian := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	ctx := merchant.WithID(context.Background(), merchant.ID(mid))
	hours, cap := 720, 7
	if strconv.IntSize == 64 {
		var large int64 = 9007199254740993
		cap = int(large)
	}
	product := models.Product{ID: uuid.New(), DisplayName: "Quoted membership", EntitlementsSpec: map[string]*int{"quota": &cap}}
	price := models.Price{ID: uuid.New(), ProductID: product.ID, Amount: 9_990_000, Currency: "USD", AutoRenew: true, AccessDurationHours: &hours}
	expiry := now.Add(time.Hour)
	amount, currency := price.Amount, price.Currency
	session := models.CheckoutSession{ID: uuid.New(), CustomerID: customer, PspID: psp, PriceID: &price.ID, Mode: models.CheckoutSessionModeSubscription, Rail: models.RailNMI, Status: models.CheckoutSessionStatusRequiresAction, Amount: &amount, Currency: &currency, ExpiresAt: &expiry}
	method := gen.OpenrailsPaymentMethod{ID: uuid.New(), MerchantID: mid, CustomerID: customer, PspID: psp, Rail: "nmi", Custodian: models.CustodianHyperSwitch, CustodianID: &custodian, RailCustomerRef: "customer", RailMethodRef: "method"}
	require.NoError(t, quoteInitialMembership(ctx, &session, &price, &product, method, now))
	// The generic RailState JSON roundtrip must not round a quoted integer cap.
	encoded, err := json.Marshal(session)
	require.NoError(t, err)
	var restored models.CheckoutSession
	require.NoError(t, json.Unmarshal(encoded, &restored))
	quoted, err := readInitialMembershipQuote(&restored)
	require.NoError(t, err)
	require.Equal(t, cap, *quoted.Entitlements["quota"])
	view := (&CheckoutSessionService{}).sessionToResponse(&restored)
	require.NotNil(t, view.MembershipQuote)
	require.Equal(t, "Quoted membership", view.MembershipQuote.ProductName)
	require.EqualValues(t, 720, view.MembershipQuote.CycleHours)
	require.Equal(t, cap, *view.MembershipQuote.Entitlements["quota"])
	*view.MembershipQuote.Entitlements["quota"] = 0
	require.Equal(t, cap, *quoted.Entitlements["quota"], "display cannot mutate the accepted quote")
	price.Amount = 123
	product.EntitlementsSpec["other"] = nil
	require.ErrorIs(t, quoteInitialMembership(ctx, &restored, &price, &product, method, now), ErrCheckoutSessionConflict)
	unchanged, err := readInitialMembershipQuote(&restored)
	require.NoError(t, err)
	require.Equal(t, quoted, unchanged)
	principal := billingauth.DelegatedPrincipal{CredentialClass: billingauth.CredentialClassUserSession, MerchantID: mid.String(), SubjectID: customer.String()}
	accepted, err := acceptedInitialMembershipQuote(ctx, &restored, principal, now.Add(10*time.Minute))
	require.NoError(t, err)
	require.Equal(t, quoted.SubscriptionID, accepted.SubscriptionID)
	require.Equal(t, quoted.PaymentID, accepted.PaymentID)
	require.Equal(t, quoted.Amount, accepted.Amount)
	require.Equal(t, quoted.Entitlements, accepted.Entitlements)
	require.Equal(t, now.Add(10*time.Minute), accepted.PeriodStart)
	require.Equal(t, 30*24*time.Hour, accepted.PeriodEnd.Sub(accepted.PeriodStart))
	for _, mutate := range []func(*billingauth.DelegatedPrincipal){
		func(p *billingauth.DelegatedPrincipal) { p.CredentialClass = billingauth.CredentialClassAutomation },
		func(p *billingauth.DelegatedPrincipal) { p.CredentialClass = billingauth.CredentialClassUnknown },
		func(p *billingauth.DelegatedPrincipal) { p.Invoker = "device" },
		func(p *billingauth.DelegatedPrincipal) { p.MerchantID = uuid.NewString() },
		func(p *billingauth.DelegatedPrincipal) { p.SubjectID = uuid.NewString() },
	} {
		bad := principal
		mutate(&bad)
		_, err := acceptedInitialMembershipQuote(ctx, &restored, bad, now)
		require.ErrorIs(t, err, ErrCheckoutSessionForbidden)
	}
	_, err = acceptedInitialMembershipQuote(ctx, &restored, principal, expiry)
	require.ErrorIs(t, err, ErrCheckoutSessionExpired)
	_, err = readInitialMembershipQuote(&restored)
	require.NoError(t, err, "expired quote remains readable for canonical operation recovery")
	changed := restored
	otherAmount := int64(1)
	changed.Amount = &otherAmount
	_, err = readInitialMembershipQuote(&changed)
	require.ErrorIs(t, err, ErrCheckoutSessionConflict)
	require.Nil(t, restored.PaymentID)
	require.Nil(t, restored.SubscriptionID, "quote construction does not claim completed payment or membership")
	for _, mutate := range []func(*models.Price, *gen.OpenrailsPaymentMethod){
		func(p *models.Price, _ *gen.OpenrailsPaymentMethod) { p.Amount = 0 },
		func(p *models.Price, _ *gen.OpenrailsPaymentMethod) { zero := int64(0); p.TrialUnitAmount = &zero },
		func(p *models.Price, _ *gen.OpenrailsPaymentMethod) { p.AutoRenew = false },
		func(p *models.Price, _ *gen.OpenrailsPaymentMethod) { p.Archived = true },
		func(_ *models.Price, m *gen.OpenrailsPaymentMethod) { m.CustomerID = uuid.New() },
		func(_ *models.Price, m *gen.OpenrailsPaymentMethod) { m.ParkReason = "pending deletion" },
	} {
		candidate := session
		candidate.RailState = nil
		p := price
		p.Amount = amount
		m := method
		mutate(&p, &m)
		require.Error(t, quoteInitialMembership(ctx, &candidate, &p, &product, m, now))
		require.Empty(t, candidate.RailState)
	}
}
