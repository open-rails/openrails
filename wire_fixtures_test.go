package openrails

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

var updateWireFixtures = flag.Bool("update-wire-fixtures", false, "rewrite testdata/wire fixtures from the Go contract")

type errorEnvelope struct {
	Error ErrorDetails `json:"error"`
}

type spendDelegationsDocument struct {
	Delegations []SpendDelegationInput `json:"delegations"`
}

// canonicalWireFixtures pins success, error, null, omitted, empty-list, list,
// time and int64-boundary money shapes. web/admin reads the same files.
func canonicalWireFixtures() map[string]any {
	when := time.Date(2026, 9, 16, 0, 0, 0, 123456789, time.UTC)
	maxMoney, minMoney, zero := int64(math.MaxInt64), int64(math.MinInt64), int64(0)
	param, sourceID := "amount", "deposit-1"
	periodHours, expMonth, expYear := 720, 12, 2030
	return map[string]any{
		"error_envelope.json": errorEnvelope{Error: ErrorDetails{
			Type: "invalid_request_error", Code: "idempotency_key_reused", Message: "retry changed the committed amount",
			RequestID: "req_fixture", Param: &param,
			Metadata: map[string]any{"committed_amount": "9223372036854775807", "attempt": json.Number("2"), "detail": nil},
		}},
		"page_empty.json": Page[CreditTransaction]{Object: "list", Data: []CreditTransaction{}, Limit: 20},
		"page_credit_transactions.json": Page[CreditTransaction]{Object: "list", Total: 2, Limit: 2, HasMore: false, Data: []CreditTransaction{
			{ID: uuid.MustParse("11111111-1111-1111-1111-111111111111"), CustomerID: "22222222-2222-2222-2222-222222222222", Invoker: "host", Currency: "USD", Amount: maxMoney, BalanceAfter: &maxMoney, TransactionType: "deposit", Status: "completed", Captured: &zero, Source: "bank", SourceID: &sourceID, ExpiresAt: &when, CreatedAt: when, UpdatedAt: when},
			{ID: uuid.MustParse("33333333-3333-3333-3333-333333333333"), CustomerID: "22222222-2222-2222-2222-222222222222", Invoker: "host", Currency: "JPY", Amount: minMoney, BalanceAfter: &minMoney, TransactionType: "withdrawal", Status: "completed", Source: "operator", CreatedAt: when, UpdatedAt: when, Replayed: true},
		}},
		"merchant_settings.json": MerchantSettings{
			InvoiceCollectionThreshold: &maxMoney, ArrearsDelinquencyFloor: &zero,
			BillingPolicies: []BillingPolicyInput{
				{Name: "credit_line", Kind: "outstanding_cap", OutstandingCapAmount: maxMoney, CollectionThresholdAmount: &minMoney},
				{Name: "monthly", Kind: "window_spend_cap", SpendWindows: []BudgetWindowInput{{Key: "month", WindowSeconds: 2592000, Limit: maxMoney, Currency: "USD"}}},
			},
			BillingPolicyBindings: []BillingPolicyBindingInput{{PolicyName: "credit_line"}, {PolicyName: "monthly", Tier: "cloud"}},
		},
		"spend_delegations.json": spendDelegationsDocument{Delegations: []SpendDelegationInput{
			{Scope: "invoker", ScopeKey: "worker-1", Windows: []SpendLimitWindow{{Key: "day", WindowSeconds: 86400, Limit: maxMoney, Currency: "USD"}}, Provenance: "sha256:fixture"},
			{Scope: "subject", Windows: []SpendLimitWindow{}},
		}},
		"hosted_checkout_session.json": HostedCheckoutSession{
			ID: "ocs_fixture", Status: "created", Merchant: HostedCheckoutMerchant{DisplayName: "Acme Demo"},
			Plan:      HostedCheckoutPlan{DisplayName: "Premium Membership", UnitAmount: maxMoney, Currency: "USD", UnitDecimals: 6, PeriodHours: &periodHours, AutomaticallyRenews: true},
			LineItems: []HostedCheckoutLineItem{{Label: "Premium Membership", Sublabel: "Renews monthly", Amount: maxMoney}, {Label: "Launch discount", Amount: minMoney}},
			Tax:       &zero, DueToday: &maxMoney,
			Rails: []HostedCheckoutRail{
				{ID: "option_card", Rail: "nmi", Mode: "subscription", Driver: "collect_js", PublicConfig: map[string]string{"tokenization_key": "public-key", "tokenization_url": "https://secure.networkmerchants.com/token/Collect.js"}},
				{ID: "option_wallet", Rail: "solana", Mode: "one_off", Driver: "solana_pay", PublicConfig: map[string]string{"token_symbol": "USDC", "network": "devnet"}},
				{ID: "option_redirect", Rail: "ccbill", Mode: "subscription", Driver: "redirect"},
			},
			SavedMethods: []HostedCheckoutSavedMethod{{ID: methodFixture.String(), OptionID: "option_card", Rail: "nmi", Brand: "visa", LastFour: "4242", ExpMonth: &expMonth, ExpYear: &expYear}},
			PaymentID:    paymentFixture.String(), SubscriptionID: subscriptionFixture.String(),
			SuccessURL: "https://merchant.example/thanks?checkout=ocs_fixture",
			ExpiresAt:  when,
		},
		"subscription.json": subscriptionFixtureValue(when, maxMoney, expMonth, expYear),
		"billing_status.json": BillingStatus{
			HasActiveSubscription: true, Subscription: ptr(subscriptionFixtureValue(when, maxMoney, expMonth, expYear)), NextRenewalAt: &when,
			Access:       &SubscriptionAccess{Kind: "subscription", Entitlement: "premium", SubscriptionID: subscriptionFixture, Rail: "nmi", StartAt: when, EndAt: &when},
			Entitlements: []EntitlementRecord{{ID: "66666666-6666-4666-8666-666666666666", CustomerID: customerFixture.String(), Entitlement: "premium", StartAt: when, EndAt: &when, SourceID: &subscriptionSource, SourceType: "subscription", CreatedAt: when, UpdatedAt: when}},
		},
		"notification.json": Notification{
			ID: uuid.MustParse("77777777-7777-4777-8777-777777777777"), CustomerID: customerFixture, EventType: "subscription_reprice_scheduled", CreatedAt: when,
			Data: NotificationData{SubscriptionID: subscriptionFixture, FromPriceID: priceFixture, ToPriceID: scheduledPriceFixture, OldAmount: &maxMoney, NewAmount: &minMoney, Currency: "USD", EffectiveAt: &when},
		},
		"catalog_price.json": CatalogPrice{ID: priceFixture, Key: "pro-monthly", ProductID: productFixture, UnitAmount: maxMoney, Currency: "USD", AutoRenew: true, CreatedAt: when, UpdatedAt: when},
		"checkout_session.json": CheckoutSession{
			ID: sessionFixture.String(), Status: "succeeded", Mode: "subscription", PriceID: new(priceFixture.String()), Amount: new(maxMoney), Currency: new("USD"), PaymentStatus: "paid",
			SubscriptionID: new(subscriptionFixture.String()), PaymentID: new(paymentFixture.String()), ExpiresAt: &when, CreatedAt: when, Metadata: map[string]string{"plan": "pro"}, RailData: map[string]any{"rail": "nmi"},
		},
		"payment.json": Payment{
			ID: paymentFixture, Object: "charge", Status: "succeeded", Amount: maxMoney, AmountRefunded: zero, Currency: "USD", CustomerID: customerFixture,
			SubscriptionID: &subscriptionFixture, Rail: "nmi", TransactionID: "txn-1", Captured: true, CreatedAt: when,
			Price: &PublicPrice{ID: priceFixture, Key: "pro-monthly", Object: "price", UnitAmount: maxMoney, Currency: "USD", Type: "recurring", Product: productFixture, Active: true, CreatedAt: when},
		},
	}
}

func ptr[T any](v T) *T { return &v }

// subscriptionFixtureValue is the self-route shape: the merchant routes serve
// the same struct without ScheduledPrice/ScheduledProduct/CancelPortalURL/Access.
func subscriptionFixtureValue(when time.Time, maxMoney int64, expMonth, expYear int) Subscription {
	portal := "https://support.ccbill.com/"
	return Subscription{
		CollectionPolicy: "provider",
		ID:               subscriptionFixture, CustomerID: customerFixture.String(), ProductID: (productFixture).String(), PriceID: (priceFixture).String(), PSPID: "55555555-5555-5555-5555-555555555555",
		Rail: "nmi", RailSubscriptionID: "rail-sub-1", Status: "active", ScheduledPriceID: ptr(scheduledPriceFixture.String()), PaymentMethodID: &methodFixture,
		StartedAt: when, CurrentPeriodStartsAt: &when, CurrentPeriodEndsAt: &when, CancelMode: "reversible", CancelPortalURL: &portal, CreatedAt: when, UpdatedAt: when,
		Price:            &SubscriptionPrice{ID: (priceFixture).String(), Key: "pro-monthly", ProductID: (productFixture).String(), UnitAmount: maxMoney, Currency: "USD", AutoRenew: true},
		Product:          &SubscriptionProduct{ID: (productFixture).String(), Key: "pro", DisplayName: "Pro"},
		ScheduledPrice:   &SubscriptionPrice{ID: (scheduledPriceFixture).String(), Key: "pro-annual", ProductID: (productFixture).String(), UnitAmount: maxMoney, Currency: "USD", AutoRenew: true},
		ScheduledProduct: &SubscriptionProduct{ID: (productFixture).String(), Key: "pro", DisplayName: "Pro"},
		Card:             &SubscriptionCard{Brand: "visa", Last4: "4242", ExpMonth: &expMonth, ExpYear: &expYear},
		Access:           &SubscriptionAccess{Kind: "subscription", Entitlement: "premium", SubscriptionID: subscriptionFixture, Rail: "nmi", StartAt: when, EndAt: &when},
		Payments:         []Payment{{ID: paymentFixture, Object: "charge", Status: "succeeded", Amount: maxMoney, Currency: "USD", CustomerID: customerFixture, SubscriptionID: &subscriptionFixture, Rail: "nmi", TransactionID: "txn-1", Captured: true, CreatedAt: when}},
	}
}

var (
	subscriptionSource    = "sub_cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	customerFixture       = CustomerID(uuid.MustParse("22222222-2222-2222-2222-222222222222"))
	productFixture        = ProductID(uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
	priceFixture          = PriceID(uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"))
	scheduledPriceFixture = PriceID(uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbc"))
	subscriptionFixture   = SubscriptionID(uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc"))
	methodFixture         = PaymentMethodID(uuid.MustParse("dddddddd-dddd-4ddd-8ddd-dddddddddddd"))
	paymentFixture        = PaymentID(uuid.MustParse("eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"))
	sessionFixture        = CheckoutSessionID(uuid.MustParse("ffffffff-ffff-4fff-8fff-ffffffffffff"))
)

func TestCanonicalWireFixtures(t *testing.T) {
	for name, value := range canonicalWireFixtures() {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var indented bytes.Buffer
			if err := json.Indent(&indented, raw, "", "  "); err != nil {
				t.Fatal(err)
			}
			indented.WriteByte('\n')
			path := filepath.Join("testdata", "wire", name)
			if *updateWireFixtures {
				if err := os.WriteFile(path, indented.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			fixture, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(fixture, indented.Bytes()) {
				t.Fatalf("%s drifted from the Go contract; review and rerun with -update-wire-fixtures:\n%s", name, indented.Bytes())
			}
			decoded := reflect.New(reflect.TypeOf(value))
			decoder := json.NewDecoder(bytes.NewReader(fixture))
			decoder.UseNumber()
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(decoded.Interface()); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(value, decoded.Elem().Interface()) {
				t.Fatalf("%s does not round-trip:\n%#v", name, decoded.Elem().Interface())
			}
			assertJavaScriptSafe(t, fixture)
		})
	}
}

// The remote Client decodes the canonical error fixture into the same details.
func TestErrorEnvelopeFixtureThroughClient(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "wire", "error_envelope.json"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write(fixture)
	}))
	defer server.Close()
	client, err := NewRemote(server.URL, WithAPIKey("fixture"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetMerchantSettings(context.Background())
	var status *StatusError
	if !errors.As(err, &status) || !errors.Is(err, ErrIdempotencyKeyReused) || !errors.Is(err, ErrConflict) {
		t.Fatalf("unexpected error %v", err)
	}
	want := canonicalWireFixtures()["error_envelope.json"].(errorEnvelope).Error
	if !reflect.DeepEqual(want, status.ErrorDetails) {
		t.Fatalf("client lost error details: %#v", status.ErrorDetails)
	}
}

// assertJavaScriptSafe rejects JSON numbers JavaScript cannot represent exactly
// and money-named fields that are not decimal strings.
func assertJavaScriptSafe(t *testing.T, raw []byte) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	var walk func(path string, value any)
	walk = func(path string, value any) {
		switch v := value.(type) {
		case map[string]any:
			_, page := v["has_more"]
			for key, child := range v {
				if _, isNumber := child.(json.Number); isNumber && moneyJSONName.MatchString(key) && !(page && key == "limit") {
					t.Errorf("%s.%s is money encoded as a JSON number", path, key)
				}
				walk(path+"."+key, child)
			}
		case []any:
			for i, child := range v {
				walk(fmt.Sprintf("%s[%d]", path, i), child)
			}
		case json.Number:
			integer, err := v.Int64()
			if err != nil || strings.ContainsAny(v.String(), ".eE") || integer > 1<<53-1 || integer < -(1<<53-1) {
				t.Errorf("%s=%s is not a JavaScript-safe integer", path, v)
			}
		}
	}
	walk("$", value)
}
