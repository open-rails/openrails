package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/railresolve"
)

const testPriceID = "price_11111111-1111-1111-1111-111111111111"

// The fingerprint is a durable replay identity: its projection is frozen,
// whitespace-insensitive where the executor trims, and CCBill hashes the
// verified email it actually sends instead of the browser's.
func TestCheckoutAttemptFingerprint(t *testing.T) {
	base := CheckoutAttemptCreateRequest{
		PriceID: testPriceID, Payment: CheckoutAttemptPaymentRequest{Rail: "stripe"},
		Metadata:   map[string]string{"post": "post-123"},
		SuccessURL: "https://app.example/success", CancelURL: "https://app.example/cancel",
	}
	// Frozen from the pre-PriceKey projection: an optional key field must not
	// strand an existing durable ID-only checkout after an upgrade.
	require.Equal(t, "5ce91979357cb141b7732e5727ae33b77e1d8892c42f3b0193d6b08002cac345", checkoutAttemptRequestFingerprint(&base))

	for name, mutate := range map[string]func(*CheckoutAttemptCreateRequest){
		"price":             func(r *CheckoutAttemptCreateRequest) { r.PriceID = "price_22222222-2222-2222-2222-222222222222" },
		"id spelled as key": func(r *CheckoutAttemptCreateRequest) { r.PriceKey, r.PriceID = r.PriceID, "" },
		"success url":       func(r *CheckoutAttemptCreateRequest) { r.SuccessURL = "https://other.example/success" },
		"cancel url":        func(r *CheckoutAttemptCreateRequest) { r.CancelURL = "https://other.example/cancel" },
		"name on card":      func(r *CheckoutAttemptCreateRequest) { r.Payment.NameOnCard = "María José" },
		"metadata value":    func(r *CheckoutAttemptCreateRequest) { r.Metadata = map[string]string{"post": "post-124"} },
		"selected amount":   func(r *CheckoutAttemptCreateRequest) { r.Amount = new(int64(100000000)) },
		"offer kind":        func(r *CheckoutAttemptCreateRequest) { r.OfferKind = billing.OfferRecurring },
	} {
		changed := base
		mutate(&changed)
		require.NotEqual(t, checkoutAttemptRequestFingerprint(&base), checkoutAttemptRequestFingerprint(&changed), name)
	}
	padded := base
	padded.SuccessURL, padded.CancelURL = "  "+base.SuccessURL+" ", " "+base.CancelURL
	padded.Metadata = map[string]string{" post ": " post-123 ", "  ": "dropped"}
	require.Equal(t, checkoutAttemptRequestFingerprint(&base), checkoutAttemptRequestFingerprint(&padded))

	ccbill := CheckoutAttemptCreateRequest{PriceID: testPriceID, Payment: CheckoutAttemptPaymentRequest{Rail: "merchant-ccbill", Email: "browser-a@example.test", NameOnCard: "Buyer", Zip: "10001", Country: "US"}}
	otherBrowser := ccbill
	otherBrowser.Payment.Email = "browser-b@example.test"
	verified, changedVerified := "verified@example.test", "changed@example.test"
	user := &UserIdentity{ID: "user_123", Email: &verified}
	require.Equal(t, checkoutAttemptRequestFingerprintForRail(&ccbill, user, "ccbill"), checkoutAttemptRequestFingerprintForRail(&otherBrowser, user, "ccbill"))
	require.NotEqual(t, checkoutAttemptRequestFingerprintForRail(&ccbill, user, "stripe"), checkoutAttemptRequestFingerprintForRail(&otherBrowser, user, "stripe"))

	// A replay decodes under the rail that executed, and a changed verified
	// identity conflicts rather than replaying another identity's form.
	response := &CheckoutAttemptResponse{Payment: CheckoutAttemptPaymentResponse{Rail: "ccbill"}}
	payload, err := json.Marshal(checkoutAttemptIdempotencyResult{RequestFingerprint: checkoutAttemptRequestFingerprintForRail(&ccbill, user, "ccbill"), Response: response})
	require.NoError(t, err)
	got, err := decodeCheckoutAttemptIdempotencyResult(payload, &otherBrowser, user)
	require.NoError(t, err)
	require.Equal(t, response, got)
	_, err = decodeCheckoutAttemptIdempotencyResult(payload, &otherBrowser, &UserIdentity{ID: "user_123", Email: &changedVerified})
	require.ErrorIs(t, err, ErrCheckoutAttemptConflict)
	require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused)
	_, err = decodeCheckoutAttemptIdempotencyResult(json.RawMessage(`{}`), &ccbill, user)
	require.Error(t, err)
}

// Replay and provider keys are stable under whitespace, scoped where they
// must be, bounded for Stripe, and never contain the raw caller key.
func TestIdempotencyKeyDerivation(t *testing.T) {
	first := stripeCheckoutIdempotencyKey(" customer:key ")
	require.Equal(t, first, stripeCheckoutIdempotencyKey("customer:key"))
	require.NotEqual(t, first, stripeCheckoutIdempotencyKey("customer:other"))
	require.Len(t, stripeCheckoutIdempotencyKey(strings.Repeat("x", 1<<10)), len(first))
	require.LessOrEqual(t, len(first), 255, "Stripe's idempotency key limit")
	require.Empty(t, stripeCheckoutIdempotencyKey("   "))

	merchantID := uuid.New()
	id := idempotentCheckoutAttemptID(merchantID, " customer:key ")
	require.Equal(t, id, idempotentCheckoutAttemptID(merchantID, "customer:key"))
	require.NotEqual(t, id, idempotentCheckoutAttemptID(merchantID, "customer:other"))
	require.NotEqual(t, id, idempotentCheckoutAttemptID(uuid.New(), "customer:key"), "merchant scoped")

	scoped := scopeIdempotencyKey(" user-1 ", " checkout-92 ")
	require.True(t, strings.HasPrefix(scoped, "user-1:"))
	require.NotContains(t, scoped, "checkout-92")
	require.NotEqual(t, scoped, scopeIdempotencyKey("user-2", "checkout-92"), "customer scoped")
	require.Equal(t, "checkout-92", scopeIdempotencyKey("", " checkout-92 "))

	price := uuid.New()
	require.NotEqual(t, GenerateKeyForSale("u", price), GenerateKeyForSubscription("u", price))
}

// SAQ A: a card number in any session field is refused loudly. Identifiers
// pass on their UUID grouping, never on a per-field exemption.
func TestCheckoutAttemptPANFirewall(t *testing.T) {
	luhnHandles := []string{
		"abcdefab-cdef-4abc-8111-111111111112",
		"a4111111-1111-4111-8119-abcdefabcdef",
		"463d4942-14ef-4eec-9436-151088257692",
		uuid.NewString(),
	}
	for _, h := range luhnHandles {
		for _, req := range []CheckoutAttemptCreateRequest{
			{PriceID: "price_" + h},
			{Payment: CheckoutAttemptPaymentRequest{PaymentMethodID: "pm_" + h}},
			{Payment: CheckoutAttemptPaymentRequest{PaymentToken: h}},
			{Metadata: map[string]string{"order_ref": h}},
			{IdempotencyKey: scopeIdempotencyKey(h, "checkout-92")},
		} {
			require.NoError(t, rejectCheckoutAttemptPAN(&req), h)
		}
		require.NoError(t, RejectPANShapedFields(&CheckoutRequest{BTTokenIntentID: h}), h)
	}
	for _, card := range []string{"4111111111111111", "5555 5555 5555 4444", "3782-822463-10005"} {
		for name, req := range map[string]CheckoutAttemptCreateRequest{
			"payment method": {Payment: CheckoutAttemptPaymentRequest{PaymentMethodID: "pm_" + card}},
			"token":          {Payment: CheckoutAttemptPaymentRequest{PaymentToken: card}},
			"name":           {Payment: CheckoutAttemptPaymentRequest{NameOnCard: "Cardholder " + card}},
			"address":        {Payment: CheckoutAttemptPaymentRequest{Address1: "PO Box " + card}},
			"wallet":         {Payment: CheckoutAttemptPaymentRequest{Wallet: card}},
			"price key":      {PriceKey: "plan-" + card},
			"success url":    {SuccessURL: "https://app.example/?n=" + card},
			"metadata value": {Metadata: map[string]string{"note": card}},
			"metadata key":   {Metadata: map[string]string{card: "note"}},
		} {
			require.ErrorIs(t, rejectCheckoutAttemptPAN(&req), ErrCheckoutAttemptValidation, "%s %s", name, card)
		}
		require.Error(t, RejectPANShapedFields(&CheckoutRequest{BTTokenIntentID: card}), card)
	}
}

func TestPriceSelectorAndOfferAssertion(t *testing.T) {
	for _, tc := range []struct {
		id, key string
		ok      bool
	}{
		{testPriceID, "", true},
		{"", "pro-monthly", true},
		{"", "", false},
		{"  ", "  ", false},
		{testPriceID, "pro-monthly", false},
		{"pro-monthly", "", false},
		{"price_00000000-0000-0000-0000-000000000000", "", false},
	} {
		productKey := ""
		if tc.key != "" {
			productKey = "pro"
		}
		err := validateCheckoutPriceSelector(tc.id, productKey, tc.key)
		if tc.ok {
			require.NoError(t, err, "%q %q", tc.id, tc.key)
		} else {
			require.ErrorIs(t, err, ErrCheckoutAttemptValidation, "%q %q", tc.id, tc.key)
		}
	}

	permanent := &models.Price{}
	finite := &models.Price{AccessDurationHours: intPtr(24)}
	recurring := &models.Price{BillingIntervalHours: intPtr(720), AccessDurationHours: intPtr(720)}
	product := &models.Product{Entitlements: []string{"forever", "timed"}}
	for _, tc := range []struct {
		name  string
		price *models.Price
		key   string
		kind  billing.OfferKind
		ok    bool
	}{
		{"no assertion", recurring, "", "", true},
		{"permanent", permanent, "forever", billing.OfferPermanent, true},
		{"finite", finite, "timed", billing.OfferFinite, true},
		{"recurring", recurring, "timed", billing.OfferRecurring, true},
		{"name does not determine duration", permanent, "timed", billing.OfferPermanent, true},
		{"finite price is not permanent", finite, "", billing.OfferPermanent, false},
		{"recurring price is not finite", recurring, "", billing.OfferFinite, false},
		{"one-off price is not recurring", finite, "", billing.OfferRecurring, false},
		{"entitlement not granted", recurring, "other", "", false},
		{"blank entitlement", recurring, "  ", "", false},
		{"NUL in entitlement", recurring, "for\x00ever", "", false},
		{"oversized entitlement", recurring, strings.Repeat("k", 257), "", false},
	} {
		err := validateOfferAssertion(tc.price, product, tc.key, tc.kind)
		if tc.ok {
			require.NoError(t, err, tc.name)
		} else {
			require.ErrorIs(t, err, ErrCheckoutAttemptValidation, tc.name)
		}
	}
}

// Blanks are "not supplied"; everything else must match an allowed origin
// exactly (host policy itself is e2e's).
func TestValidateReturnURLs(t *testing.T) {
	svc := &CheckoutAttemptService{config: &config.Config{PublicBillingBaseURL: "https://billing.example/api"}}
	require.NoError(t, svc.validateReturnURLs("", "  ", "https://billing.example/done"))
	for _, bad := range []string{"https://user@billing.example/done", "https://billing.example.evil/done", "/relative", "javascript:alert(1)"} {
		require.ErrorIs(t, svc.validateReturnURLs(bad), ErrCheckoutAttemptValidation, bad)
	}
	require.Error(t, (&CheckoutAttemptService{}).validateReturnURLs("https://billing.example/done"), "no configured origin refuses all")
	var nilSvc *CheckoutAttemptService
	require.Error(t, nilSvc.validateReturnURLs("https://billing.example/done"))
}

// Per-rail input contracts match what each executor consumes: CCBill needs
// only name, postal code, ISO country and the VERIFIED email.
func TestValidatePaymentPerRail(t *testing.T) {
	verified := "buyer@example.test"
	user := &UserIdentity{ID: "user_123", Email: &verified}
	svc := &CheckoutAttemptService{}
	pmID := "pm_11111111-1111-1111-1111-111111111111"
	for _, tc := range []struct {
		name, rail string
		payment    CheckoutAttemptPaymentRequest
		user       *UserIdentity
		want       string
	}{
		{"stripe hosted needs nothing", "stripe", CheckoutAttemptPaymentRequest{}, user, ""},
		{"stripe saved method needs ownership service", "stripe", CheckoutAttemptPaymentRequest{PaymentMethodID: pmID}, user, "payment method service unavailable"},
		{"nmi token", "nmi", CheckoutAttemptPaymentRequest{PaymentToken: "tok"}, user, ""},
		{"nmi neither", "nmi", CheckoutAttemptPaymentRequest{}, user, "payment_method_id or payment_token is required"},
		{"nmi both", "nmi", CheckoutAttemptPaymentRequest{PaymentToken: "tok", PaymentMethodID: pmID}, user, "either payment_token or payment_method_id"},
		{"nmi malformed method", "nmi", CheckoutAttemptPaymentRequest{PaymentMethodID: "card_1"}, user, "invalid payment_method_id"},
		{"ccbill minimal identity", "ccbill", CheckoutAttemptPaymentRequest{NameOnCard: "Prince", Zip: " 55401 ", Country: " us "}, user, ""},
		{"ccbill name", "ccbill", CheckoutAttemptPaymentRequest{Zip: "1", Country: "US"}, user, "name_on_card"},
		{"ccbill postal", "ccbill", CheckoutAttemptPaymentRequest{NameOnCard: "B", Country: "US"}, user, "zip"},
		{"ccbill country letters", "ccbill", CheckoutAttemptPaymentRequest{NameOnCard: "B", Zip: "1", Country: "12"}, user, "country"},
		{"ccbill country length", "ccbill", CheckoutAttemptPaymentRequest{NameOnCard: "B", Zip: "1", Country: "USA"}, user, "country"},
		{"ccbill unverified email", "ccbill", CheckoutAttemptPaymentRequest{NameOnCard: "B", Zip: "1", Country: "US"}, &UserIdentity{ID: "u"}, "verified email"},
		{"unsupported rail", "paypal", CheckoutAttemptPaymentRequest{}, user, "unsupported rail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.validatePayment(context.Background(), tc.rail, &tc.payment, tc.user)
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrCheckoutAttemptValidation)
			require.ErrorContains(t, err, tc.want)
			require.Equal(t, tc.name == "nmi neither", errors.Is(err, ErrPaymentMethodRequired))
		})
	}

	payment := &CheckoutAttemptPaymentRequest{Email: "spoofed@example.test", NameOnCard: "Prince", Zip: " 55401 ", Country: " us "}
	require.NoError(t, svc.validateCCBillInput(payment, user))
	require.Equal(t, "55401", payment.Zip)
	require.Equal(t, "US", payment.Country)
	fields := svc.buildRailFields("ccbill", payment, user)
	require.Equal(t, verified, fields["email"], "the verified email wins over browser input")
	require.NotContains(t, fields, "address1")
}

func TestCanonicalizeCheckoutPaymentName(t *testing.T) {
	full := CheckoutAttemptPaymentRequest{NameOnCard: "  李  小龍  ", FirstName: "ignored", LastName: "legacy"}
	canonicalizeCheckoutPaymentName(&full)
	require.Equal(t, "李  小龍", full.NameOnCard, "internal spacing is preserved")
	require.Equal(t, "李", full.FirstName)
	require.Equal(t, "小龍", full.LastName)

	split := CheckoutAttemptPaymentRequest{FirstName: " María de ", LastName: "la Vega"}
	canonicalizeCheckoutPaymentName(&split)
	require.Equal(t, "María de la Vega", split.NameOnCard)
	require.Equal(t, "María de", split.FirstName, "an explicit split is kept")
}

type capturingExecutor struct {
	captured *CheckoutRequest
	resp     *CheckoutResponse
}

func (c *capturingExecutor) resolveRailTarget(_ context.Context, selector string) (railTarget, error) {
	return railTarget{PSP: selector, Rail: selector}, nil
}
func (c *capturingExecutor) pspKeyArchived(context.Context, string) bool { return false }
func (c *capturingExecutor) railSource() railresolve.Source              { return railresolve.FixedSet{} }
func (c *capturingExecutor) Checkout(_ context.Context, req *CheckoutRequest, _ *UserIdentity) (*CheckoutResponse, error) {
	c.captured = req
	if c.resp == nil {
		return nil, errors.New("stop after capture")
	}
	return c.resp, nil
}
func (c *capturingExecutor) RegisterPurchase(context.Context, *payments.RegisterPurchaseRequest) (*payments.RegisterPurchaseResponse, error) {
	return nil, nil
}
func (c *capturingExecutor) CheckSubscriptionConflict(context.Context, string, *models.Price, *models.Product) (*SubscriptionConflict, error) {
	return &SubscriptionConflict{}, nil
}

// The session hands the executor its return URLs, start time and pinned PSP,
// under a session-derived provider key, and maps the outcome.
func TestInitializeCheckoutAttempt(t *testing.T) {
	started := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	session := func() *models.CheckoutAttempt {
		return &models.CheckoutAttempt{ID: uuid.New(), PriceID: new(uuid.New()), Rail: models.RailNMI, CreatedAt: started, RailFields: map[string]any{checkoutAttemptPSPFieldKey: " mobius "}}
	}
	exec := &capturingExecutor{}
	svc := &CheckoutAttemptService{checkoutService: exec}
	s := session()
	_ = svc.initializeCheckoutAttempt(context.Background(), s, &CheckoutAttemptPaymentRequest{PaymentToken: "tok"}, "https://app.example/ok", "https://app.example/no", &UserIdentity{ID: "u"})
	require.NotNil(t, exec.captured)
	require.Equal(t, "https://app.example/ok", exec.captured.SuccessURL)
	require.Equal(t, "https://app.example/no", exec.captured.CancelURL)
	require.Equal(t, started, exec.captured.CheckoutStartedAt)
	require.Equal(t, "mobius", exec.captured.Rail, "the pinned PSP, never a re-resolved rail kind")
	require.Equal(t, "checkout_native_session:"+s.ID.String(), exec.captured.IdempotencyKey)

	for _, tc := range []struct {
		resp   CheckoutResponse
		status models.CheckoutAttemptStatus
		err    error
	}{
		{CheckoutResponse{Status: "success"}, models.CheckoutAttemptStatusSucceeded, nil},
		{CheckoutResponse{Status: "pending"}, models.CheckoutAttemptStatusSucceeded, nil},
		{CheckoutResponse{Status: "redirect_required", RedirectURL: " https://pay.example/r "}, models.CheckoutAttemptStatusRequiresAction, nil},
		{CheckoutResponse{Status: "redirect_required"}, "", ErrCheckoutAttemptValidation},
		{CheckoutResponse{Status: "blocked", Message: "already subscribed"}, "", ErrCheckoutAttemptConflict},
		{CheckoutResponse{Status: "declined"}, "", ErrCheckoutAttemptConflict},
	} {
		s := session()
		exec.resp = &tc.resp
		err := svc.initializeCheckoutAttempt(context.Background(), s, &CheckoutAttemptPaymentRequest{}, "", "", &UserIdentity{ID: "u"})
		require.ErrorIs(t, err, tc.err, tc.resp.Status)
		if tc.err == nil {
			require.Equal(t, tc.status, s.Status, tc.resp.Status)
		}
		if tc.status == models.CheckoutAttemptStatusRequiresAction {
			require.Equal(t, "https://pay.example/r", s.RailState["redirect_url"])
		}
	}
}

type failingCompleter struct{ err error }

func (f failingCompleter) Complete(context.Context, json.RawMessage) error {
	return f.err
}

// A failed completion is logged with its key but never with the result
// payload, which can carry provider data.
func TestCompleteCheckoutIdempotencyNeverLogsResult(t *testing.T) {
	hook := logtest.NewGlobal()
	t.Cleanup(hook.Reset)
	const secret = "sensitive-result-must-not-be-logged"
	completeCheckoutIdempotency(context.Background(), failingCompleter{errors.New("redis unavailable")}, "checkout_attempt", "idem-key", json.RawMessage(`{"p":"`+secret+`"}`))

	var entry *log.Entry
	for _, e := range hook.AllEntries() {
		if e.Message == "checkout idempotency completion failed" {
			entry = e
		}
	}
	require.NotNil(t, entry)
	require.Equal(t, log.ErrorLevel, entry.Level)
	require.Equal(t, "idem-key", entry.Data["idempotency_key"])
	require.NotContains(t, entry.Message+fmt.Sprint(entry.Data), secret)
}
