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

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/modules/checkoutsession"
)

var updateWireFixtures = flag.Bool("update-wire-fixtures", false, "rewrite testdata/wire fixtures from the Go contract")

type errorEnvelope struct {
	Error billing.ErrorDetails `json:"error"`
}

// canonicalWireFixtures pins success, error, null, omitted, empty-list, list,
// time and int64-boundary money shapes. web/admin reads the same files.
func canonicalWireFixtures() map[string]any {
	when := time.Date(2026, 9, 16, 0, 0, 0, 123456789, time.UTC)
	maxMoney, minMoney, zero := int64(math.MaxInt64), int64(math.MinInt64), int64(0)
	param, sourceID := "amount", "deposit-1"
	periodHours, expMonth, expYear := 720, 12, 2030
	card := &billing.CardDetails{Brand: ptr("visa"), Last4: ptr("4242"), ExpMonth: &expMonth, ExpYear: &expYear}
	priceFixtureValue := billing.Price{ID: priceFixture, Key: "pro-monthly", ProductID: productFixture, UnitAmount: maxMoney, Currency: "USD", AccessDurationHours: &periodHours, BillingIntervalHours: &periodHours,
		PSPs: map[string]billing.PSPLinkState{"mobius": {Status: billing.PSPLinkLinked, SyncStatus: billing.SyncStatusUnknown}}, PendingManualActions: []billing.PendingAction{}, CreatedAt: when, UpdatedAt: when}
	return map[string]any{
		"error_envelope.json": errorEnvelope{Error: billing.ErrorDetails{
			Type: "invalid_request_error", Code: "idempotency_key_reused", Message: "retry changed the committed amount",
			RequestID: "req_fixture", Param: &param,
			Metadata: map[string]any{"committed_amount": "9223372036854775807", "attempt": json.Number("2"), "detail": nil},
		}},
		"page_empty.json": billing.ListPage[billing.CreditTransaction]{Items: []billing.CreditTransaction{}},
		"page_credit_transactions.json": billing.ListPage[billing.CreditTransaction]{Next: "next-page", Items: []billing.CreditTransaction{
			{ID: billing.CreditTransactionID(uuid.MustParse("11111111-1111-1111-1111-111111111111")), CustomerID: customerFixture, Currency: "USD", Type: billing.CreditDeposit, Amount: maxMoney, CreditGrantID: &grantFixture, Invoker: &invokerFixture, Source: "grant", SourceID: sourceID, CreatedAt: when},
			{ID: billing.CreditTransactionID(uuid.MustParse("33333333-3333-3333-3333-333333333333")), CustomerID: customerFixture, Currency: "JPY", Type: billing.CreditSpend, Amount: minMoney, Source: "operator", SourceID: "spend-1", CreatedAt: when},
		}},
		"merchant_settings.json": billing.MerchantSettings{
			InvoiceCollectionThreshold: &maxMoney, ArrearsDelinquencyFloor: &zero,
			BillingPolicies: []billing.BillingPolicy{
				{Name: "credit_line", Kind: "outstanding_cap", OutstandingCapAmount: maxMoney, CollectionThresholdAmount: &minMoney},
				{Name: "monthly", Kind: "window_spend_cap", SpendWindows: []billing.BudgetWindow{{Key: "month", WindowSeconds: 2592000, Limit: maxMoney, Currency: "USD"}}},
			},
			BillingPolicyBindings: []billing.BillingPolicyBinding{{PolicyName: "credit_line"}, {PolicyName: "monthly", Tier: "cloud"}},
		},
		"spend_delegations.json": billing.ListPage[billing.SpendDelegation]{Items: []billing.SpendDelegation{
			{Scope: billing.SpendDelegationInvoker, ScopeKey: "worker-1", Windows: []billing.BudgetWindow{{Key: "day", WindowSeconds: 86400, Limit: maxMoney, Currency: "USD"}}, Provenance: ptr("sha256:fixture")},
			{Scope: billing.SpendDelegationInvokerTier, ScopeKey: "free", Windows: []billing.BudgetWindow{}},
		}},
		"checkout_session.json": checkoutsession.CheckoutSession{
			ID: "ocs_fixture", Status: "requires_action", Merchant: checkoutsession.CheckoutSessionMerchant{DisplayName: "Acme Demo"},
			Plan:      checkoutsession.CheckoutSessionPlan{DisplayName: "Premium Membership", UnitAmount: maxMoney, Currency: "USD", UnitDecimals: 6, BillingIntervalHours: &periodHours, AccessDurationHours: &periodHours},
			LineItems: []checkoutsession.CheckoutSessionLineItem{{Label: "Premium Membership", Sublabel: ptr("Renews monthly"), Amount: maxMoney}, {Label: "Launch discount", Amount: minMoney}},
			Tax:       &zero, DueToday: &maxMoney,
			Options: []checkoutsession.CheckoutSessionOption{
				{ID: "option_card", PSPID: billing.PSPID(uuid.MustParse("55555555-5555-5555-5555-555555555555")), Rail: "nmi", Mode: "subscription", Driver: "collect_js", PublicConfig: map[string]string{"tokenization_key": "public-key", "tokenization_url": "https://secure.networkmerchants.com/token/Collect.js"}},
				{ID: "option_wallet", PSPID: billing.PSPID(uuid.MustParse("66666666-6666-4666-8666-666666666666")), Rail: "solana", Mode: "one_off", Driver: "solana_pay", PublicConfig: map[string]string{"token_symbol": "USDC", "network": "devnet"}},
				{ID: "option_elements", PSPID: billing.PSPID(uuid.MustParse("77777777-7777-4777-8777-777777777777")), Rail: "stripe", Mode: "subscription", Driver: "stripe_elements", PublicConfig: map[string]string{"publishable_key": "pk_test_fixture"}},
			},
			SavedMethods: []checkoutsession.CheckoutSessionSavedMethod{{ID: methodFixture, OptionID: "option_card", Rail: "nmi", Card: card}},
			NextAction:   &billing.NextAction{Type: "solana_pay", URL: ptr("solana:https://pay.example/billing/v1/checkout-attempts/chk_ffffffff-ffff-4fff-8fff-ffffffffffff/solana-pay")},
			Operation:    &billing.PaymentOperation{ID: billing.PaymentOperationID(uuid.MustParse("88888888-8888-4888-8888-888888888888")), Status: "pending"},
			SuccessURL:   ptr("https://merchant.example/thanks?checkout=ocs_fixture"),
			ExpiresAt:    when,
		},
		"subscription.json": subscriptionFixtureValue(when, priceFixtureValue, card),
		"notification.json": billing.Notification{
			ID: billing.NotificationID(uuid.MustParse("77777777-7777-4777-8777-777777777777")), CustomerID: billing.CustomerID((customerFixture)), EventType: "subscription_reprice_scheduled", CreatedAt: when,
			Data: billing.NotificationData{SubscriptionID: subscriptionFixture, FromPriceID: billing.PriceID((priceFixture)), ToPriceID: billing.PriceID((scheduledPriceFixture)), OldAmount: &maxMoney, NewAmount: &minMoney, Currency: "USD", EffectiveAt: &when},
		},
		"price.json": priceFixtureValue,
		"product.json": billing.Product{ID: productFixture, Key: "pro", DisplayName: "Pro", EntitlementsSpec: map[string]*int{"pro": nil},
			Prices: []billing.Price{priceFixtureValue}, CreatedAt: when, UpdatedAt: when},
		"checkout_attempt.json": billing.CheckoutAttempt{ID: sessionFixture, CustomerID: customerFixture, Status: billing.CheckoutAttemptSucceeded, Mode: "subscription",
			PriceID: &priceFixture, Amount: new(maxMoney), Currency: new("USD"), SubscriptionID: &subscriptionFixture, PaymentID: &paymentFixture,
			ExpiresAt: &when, CreatedAt: when, Metadata: map[string]string{"plan": "pro"},
		},
		"payment.json": paymentFixtureValue(when, priceFixtureValue, card),
		"payment_method.json": billing.PaymentMethod{
			ID: methodFixture, CustomerID: customerFixture, Rail: "nmi", PSPID: ptr(billing.PSPID(uuid.MustParse("55555555-5555-5555-5555-555555555555"))), Card: card,
			BillingDetails:       &billing.BillingDetails{Name: ptr("Ada Lovelace"), Address: &billing.BillingAddress{PostalCode: ptr("80202"), Country: ptr("US")}},
			Health:               billing.PaymentMethodHealth{ExpiryStatus: ptr(billing.CardExpiryValid), LastChargedAt: &when, LastChargeOutcome: ptr(billing.ChargeSucceeded), Active: true},
			Subscriptions:        []billing.PaymentMethodSubscription{{ID: subscriptionFixture, DisplayName: "Pro", CreatedAt: when}},
			CollectionCurrencies: []string{"USD"}, CreatedAt: when,
		},
		"page_invoices.json": billing.ListPage[billing.Invoice]{Next: "cursor-2", Items: []billing.Invoice{{
			ID: invoiceFixture, CustomerID: customerFixture, Currency: "USD", PeriodStartsAt: when, PeriodEndsAt: when, TotalAmount: maxMoney, AmountDue: maxMoney,
			LineItems: []billing.InvoiceLineItem{{EventType: "tokens", Amount: maxMoney, Count: 3, Dimensions: map[string]int64{"input": 2}}}, MoneyMovements: billing.AmountMap{"usage": maxMoney},
			Tax: map[string]any{}, BillingContacts: []billing.InvoiceContact{{Name: "Ops", Email: "ops@example.test"}},
			Status: billing.InvoiceOpen, CollectionMethod: billing.CollectChargeAutomatically, DueAt: &when,
			AvailableActions: []billing.InvoiceAction{billing.InvoiceActionRetryCollection, billing.InvoiceActionVoid}, CreatedAt: when,
		}}},
	}
}

func ptr[T any](v T) *T { return &v }

// subscriptionFixtureValue is the self-route shape: the merchant routes serve
// the same struct without ScheduledPrice/ScheduledProduct/CancelPortalURL/Access.
func subscriptionFixtureValue(when time.Time, price billing.Price, card *billing.CardDetails) billing.Subscription {
	portal := "https://support.ccbill.com/"
	scheduled := price
	scheduled.ID, scheduled.Key = scheduledPriceFixture, "pro-annual"
	return billing.Subscription{
		CollectionPolicy: "provider",
		ID:               subscriptionFixture, CustomerID: customerFixture, ProductID: productFixture, PriceID: priceFixture, PSPID: billing.PSPID(uuid.MustParse("55555555-5555-5555-5555-555555555555")),
		Rail: "nmi", RailSubscriptionID: ptr("rail-sub-1"), Status: "active", ScheduledPriceID: ptr(scheduledPriceFixture), PaymentMethodID: &methodFixture,
		StartedAt: when, CurrentPeriodStartsAt: &when, CurrentPeriodEndsAt: &when, CancelMode: "reversible", CancelPortalURL: &portal, CreatedAt: when, UpdatedAt: when,
		Price:            &price,
		Product:          &billing.ProductSummary{ID: productFixture, Key: "pro", DisplayName: "Pro"},
		ScheduledPrice:   &scheduled,
		ScheduledProduct: &billing.ProductSummary{ID: productFixture, Key: "pro", DisplayName: "Pro"},
		Card:             card,
		Access:           &billing.SubscriptionAccess{Kind: "subscription", Entitlement: "premium", SubscriptionID: subscriptionFixture, Rail: "nmi", StartsAt: when, EndsAt: &when},
		Payments:         []billing.Payment{paymentFixtureValue(when, price, card)},
	}
}

func paymentFixtureValue(when time.Time, price billing.Price, card *billing.CardDetails) billing.Payment {
	return billing.Payment{
		ID: paymentFixture, Kind: billing.PaymentCharge, Status: billing.PaymentSucceeded, Amount: price.UnitAmount, Currency: "USD", CustomerID: customerFixture,
		SubscriptionID: &subscriptionFixture, PriceID: priceFixture, Product: &billing.ProductSummary{ID: productFixture, Key: "pro", DisplayName: "Pro"},
		Price:   &price,
		Channel: billing.ChannelRail, Rail: ptr("nmi"), PSPID: ptr(billing.PSPID(uuid.MustParse("55555555-5555-5555-5555-555555555555"))), TransactionID: "txn-1", Card: card, CreatedAt: when,
	}
}

var (
	grantFixture          = billing.CreditGrantID(uuid.MustParse("99999999-9999-4999-8999-999999999999"))
	invokerFixture        = "host"
	customerFixture       = billing.CustomerID(uuid.MustParse("22222222-2222-2222-2222-222222222222"))
	productFixture        = billing.ProductID(uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
	priceFixture          = billing.PriceID(uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"))
	scheduledPriceFixture = billing.PriceID(uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbc"))
	subscriptionFixture   = billing.SubscriptionID(uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc"))
	methodFixture         = billing.PaymentMethodID(uuid.MustParse("dddddddd-dddd-4ddd-8ddd-dddddddddddd"))
	paymentFixture        = billing.PaymentID(uuid.MustParse("eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"))
	sessionFixture        = billing.CheckoutAttemptID(uuid.MustParse("ffffffff-ffff-4fff-8fff-ffffffffffff"))
	invoiceFixture        = billing.InvoiceID(uuid.MustParse("99999999-9999-4999-8999-999999999999"))
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
	client, err := NewRemote(server.URL, WithAPIKey("fixture"), WithDefaultMerchant("fixture"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetMerchantConfiguration(context.Background())
	var status *billing.StatusError
	if !errors.As(err, &status) || !errors.Is(err, billing.ErrIdempotencyKeyReused) || !errors.Is(err, billing.ErrConflict) {
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
