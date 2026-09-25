package openrails

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type recordedRequest struct {
	method, path, query string
	body                map[string]any
}

// recordingRemote replies with the canned body per path (default {}) and records requests.
func recordingRemote(t *testing.T, replies map[string]string) (*Client, chan recordedRequest) {
	t.Helper()
	seen := make(chan recordedRequest, 16)
	client := newTestRemote(t, func(w http.ResponseWriter, r *http.Request) {
		got := recordedRequest{method: r.Method, path: r.URL.EscapedPath(), query: r.URL.RawQuery}
		raw, _ := io.ReadAll(r.Body)
		if len(raw) > 0 {
			require.NoError(t, json.Unmarshal(raw, &got.body))
		}
		seen <- got
		reply, ok := replies[r.URL.Path]
		if !ok {
			reply = `{}`
		}
		_, _ = w.Write([]byte(reply))
	})
	return client, seen
}

func TestClientRequestShapes(t *testing.T) {
	customer := CustomerID(uuid.MustParse("7d5b4a0e-8c3f-4c1e-9b2a-1f0e2d3c4b5a")).String()
	product := ProductID(uuid.New()).String()
	client, seen := recordingRemote(t, map[string]string{
		"/v2/merchant/admissions":                                  `{"items":[{"status":200,"result":{"allowed":true}}]}`,
		"/v2/merchant/trust-level":                                 `{"currency":"USD","trust_level":"gold"}`,
		"/v2/merchant/credits/deposit":                             `{"amount":"1"}`,
		"/v2/merchant/users/" + customer + "/product-access":       `{"customer_id":"` + customer + `","product_id":"` + product + `","has_access":true,"data":[],"has_more":true,"next_cursor":"next"}`,
		"/v2/merchant/users/" + customer + "/product-access/check": `{"access":{"` + product + `":true}}`,
	})
	key := "operation-key"
	who := CheckoutCustomerIdentity{ID: customer}
	expires := time.Now().Add(time.Hour)
	window := []SpendLimitWindow{{Key: "month", WindowSeconds: 2592000, Limit: 42, Currency: "USD"}}
	cases := []struct {
		name   string
		call   func() error
		method string
		path   string
		query  string
		check  func(t *testing.T, body map[string]any)
	}{
		{"checkout purchase", func() error {
			_, err := client.CreateCheckoutSession(t.Context(), CreateCheckoutSessionRequest{Customer: who, PriceID: PriceID(uuid.New()).String(), IdempotencyKey: key})
			return err
		}, http.MethodPost, "/v2/merchant/checkout-sessions", "", func(t *testing.T, b map[string]any) {
			require.Contains(t, b, "price_id")
			require.NotContains(t, b, "subscription_id")
			require.NotContains(t, b, "mode", "the endpoint selects the operation")
		}},
		{"payment method session", func() error {
			_, err := client.CreatePaymentMethodSession(t.Context(), CreatePaymentMethodSessionRequest{Customer: who, IdempotencyKey: key})
			return err
		}, http.MethodPost, "/v2/merchant/payment-method-sessions", "", func(t *testing.T, b map[string]any) {
			require.NotContains(t, b, "price_id")
			require.NotContains(t, b, "mode")
		}},
		{"solana cancel", func() error {
			_, err := client.CreateSolanaCancelSession(t.Context(), CreateSolanaCancelSessionRequest{Customer: who, SubscriptionID: SubscriptionID(uuid.New()).String(), IdempotencyKey: key})
			return err
		}, http.MethodPost, "/v2/merchant/solana-cancel-sessions", "", func(t *testing.T, b map[string]any) {
			require.Contains(t, b, "subscription_id")
			require.NotContains(t, b, "price_id")
			require.NotContains(t, b, "new_price_id")
		}},
		{"solana tier change", func() error {
			_, err := client.CreateSolanaTierChangeSession(t.Context(), CreateSolanaTierChangeSessionRequest{Customer: who, SubscriptionID: SubscriptionID(uuid.New()).String(), NewPriceID: PriceID(uuid.New()).String(), IdempotencyKey: key})
			return err
		}, http.MethodPost, "/v2/merchant/solana-tier-change-sessions", "", func(t *testing.T, b map[string]any) {
			require.Contains(t, b, "subscription_id")
			require.Contains(t, b, "new_price_id")
		}},
		{"settings carry named policies and tier bindings", func() error {
			return client.SetMerchantSettings(t.Context(), MerchantSettings{
				BillingPolicies:       []BillingPolicyInput{{Name: "api_line", Kind: "outstanding_cap", OutstandingCapAmount: 200_000_000}},
				BillingPolicyBindings: []BillingPolicyBindingInput{{PolicyName: "api_line", Tier: "gold"}},
			})
		}, http.MethodPut, "/v2/merchant/settings", "", func(t *testing.T, b map[string]any) {
			require.Equal(t, "api_line", b["billing_policies"].([]any)[0].(map[string]any)["name"])
			binding := b["billing_policy_bindings"].([]any)[0].(map[string]any)
			require.Equal(t, "api_line", binding["policy"])
			require.Equal(t, "gold", binding["tier"])
		}},
		{"admission carries trust level and prospective rate", func() error {
			_, err := client.AdmitBatch(t.Context(), []AdmitRequest{{CustomerID: customer, TrustLevel: "gold", EstimatedAmount: 1, ExpiresAt: &expires, RequestID: "req_1", AccrualRateDeltaPerHour: 42}})
			return err
		}, http.MethodPost, "/v2/merchant/admissions", "", func(t *testing.T, b map[string]any) {
			item := b["items"].([]any)[0].(map[string]any)
			require.Equal(t, "gold", item["trust_level"])
			require.Equal(t, "42", item["accrual_rate_delta_per_hour"])
		}},
		{"trust level read", func() error {
			level, err := client.GetTrustLevel(t.Context(), customer, " USD ")
			if err == nil && level != "gold" {
				err = errors.New("trust level not decoded: " + level)
			}
			return err
		}, http.MethodGet, "/v2/merchant/trust-level", "currency=USD&customer_id=" + customer, nil},
		{"deposit keys are opaque query values", func() error {
			receipt, err := client.GetDeposit(t.Context(), customer, "../source/receipt?part=1&currency=JPY")
			if err == nil && receipt.Amount != 1 {
				err = errors.New("deposit amount not decoded")
			}
			return err
		}, http.MethodGet, "/v2/merchant/credits/deposit", "customer_id=" + customer + "&source_id=..%2Fsource%2Freceipt%3Fpart%3D1%26currency%3DJPY", nil},
		{"delegation document replace", func() error {
			return client.SetCustomerSpendDelegations(t.Context(), customer, []SpendDelegationInput{{Scope: "invoker", ScopeKey: "invoker-1", Windows: window}})
		}, http.MethodPut, "/v2/merchant/customers/" + customer + "/spend-delegations", "", func(t *testing.T, b map[string]any) {
			require.Len(t, b["delegations"], 1)
		}},
		{"delegation upsert", func() error {
			return client.SetCustomerSpendDelegation(t.Context(), customer, SpendDelegationInput{Scope: "invoker", ScopeKey: "issuer:subject", Windows: window})
		}, http.MethodPut, "/v2/merchant/customers/" + customer + "/spend-delegations:upsert", "", func(t *testing.T, b map[string]any) {
			require.Equal(t, "issuer:subject", b["scope_key"])
		}},
		{"delegation delete escapes one segment per key", func() error {
			return client.DeleteCustomerSpendDelegation(t.Context(), customer, " invoker ", "a/b:c")
		}, http.MethodDelete, "/v2/merchant/customers/" + customer + "/spend-delegations/invoker/a%2Fb:c", "", nil},
		{"access check by id", func() error {
			got, err := client.ProductAccess.Check(t.Context(), &ProductAccessCheckParams{CustomerID: customer, ProductID: product})
			if err == nil && !got.HasAccess {
				err = errors.New("access not decoded")
			}
			return err
		}, http.MethodGet, "/v2/merchant/users/" + customer + "/product-access", "product_id=" + product, nil},
		{"access check by key", func() error {
			_, err := client.ProductAccess.Check(t.Context(), &ProductAccessCheckParams{CustomerID: customer, ProductKey: "pro plan&x"})
			return err
		}, http.MethodGet, "/v2/merchant/users/" + customer + "/product-access", "product_key=pro+plan%26x", nil},
		{"access batch keeps duplicates", func() error {
			got, err := client.ProductAccess.CheckMany(t.Context(), &ProductAccessCheckManyParams{CustomerID: customer, ProductIDs: []string{product, product}})
			if err == nil && !got[product] {
				err = errors.New("batch access not decoded")
			}
			return err
		}, http.MethodPost, "/v2/merchant/users/" + customer + "/product-access/check", "", func(t *testing.T, b map[string]any) {
			require.Equal(t, []any{product, product}, b["product_ids"])
		}},
		{"access list pages", func() error {
			page, err := client.ProductAccess.List(t.Context(), &ProductAccessListParams{CustomerID: customer, Limit: 7, Cursor: "cursor"})
			if err == nil && (!page.HasMore || page.NextCursor != "next") {
				err = errors.New("page not decoded")
			}
			return err
		}, http.MethodGet, "/v2/merchant/users/" + customer + "/product-access", "cursor=cursor&limit=7", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, tc.call())
			got := <-seen
			require.Equal(t, tc.method, got.method)
			require.Equal(t, tc.path, got.path)
			require.Equal(t, tc.query, got.query)
			if tc.check != nil {
				tc.check(t, got.body)
			}
		})
	}
	empty, err := client.ProductAccess.CheckMany(t.Context(), &ProductAccessCheckManyParams{CustomerID: customer, ProductIDs: []string{}})
	require.NoError(t, err)
	require.Empty(t, empty, "an empty batch is answered locally")
	require.Empty(t, seen)
}

// failingTransport fails the test if the Client sends anything.
func failingTransport(t *testing.T) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("client sent %s %s for invalid input", r.Method, r.URL.Path)
		return nil, errors.New("no request expected")
	})}
}

// Invalid identifiers are refused before any I/O with the server's own
// invalid_param StatusError, so embedded and remote callers see one error.
func TestClientRefusesInvalidIdentifiersBeforeIO(t *testing.T) {
	ctx := context.Background()
	c, err := NewRemote("http://openrails.invalid", WithDefaultMerchant("fixture"), WithHTTPClient(failingTransport(t)),
		WithTokenProvider(func(context.Context) (string, error) { return "", errors.New("token provider must not run") }))
	require.NoError(t, err)
	now := time.Now()
	customer, price, subscription, method := CustomerID(uuid.New()).String(), PriceID(uuid.New()), SubscriptionID(uuid.New()), PaymentMethodID(uuid.New())
	noCustomer, noPrice := CustomerID{}.String(), PriceID{}.String()

	// Free-form host strings: blank and dot segments are refused.
	pathStrings := map[string]func(id string) error{
		"product key":        func(id string) error { _, err := c.Products.RetrieveByKey(ctx, id); return err },
		"price key":          func(id string) error { _, err := c.Prices.RetrieveByKey(ctx, id); return err },
		"set price key":      func(id string) error { _, err := c.Prices.SetKey(ctx, price.String(), id); return err },
		"usage meter":        func(id string) error { _, err := c.GetUsageMeter(ctx, id); return err },
		"ensure usage meter": func(id string) error { return c.EnsureUsageMeter(ctx, UsageMeterSpec{Key: id}) },
		"set rate card": func(id string) error {
			_, err := c.SetDefaultUsageRateCard(ctx, id, DefaultUsageRateCardRequest{})
			return err
		},
		"delete rate card": func(id string) error { return c.DeleteDefaultUsageRateCard(ctx, id) },
		"delete delegation scope": func(id string) error {
			return c.DeleteCustomerSpendDelegation(ctx, customer, id, "key")
		},
		"delete delegation key": func(id string) error {
			return c.DeleteCustomerSpendDelegation(ctx, customer, "invoker", id)
		},
		"grant entitlement": func(id string) error {
			_, err := c.GrantEntitlement(ctx, customer, GrantEntitlementRequest{Entitlement: id})
			return err
		},
		"revoke entitlement":      func(id string) error { return c.RevokeEntitlement(ctx, customer, id) },
		"operation authorization": func(id string) error { _, err := c.GetOperationAuthorization(ctx, id); return err },
		"billing qualification":   func(id string) error { _, err := c.GetProviderBillingQualification(ctx, id); return err },
	}
	// Opaque host strings (request ids, deposit keys, migration prices): only blankness is refused.
	blankOnly := map[string]func(id string) error{
		"preview migration source": func(id string) error {
			_, err := c.PreviewPlanMigration(ctx, PlanMigrationRequest{SourcePrice: id, TargetPrice: "b"})
			return err
		},
		"preview migration target": func(id string) error {
			_, err := c.PreviewPlanMigration(ctx, PlanMigrationRequest{SourcePrice: "a", TargetPrice: id})
			return err
		},
		"create migration source": func(id string) error {
			_, err := c.CreatePlanMigration(ctx, PlanMigrationRequest{SourcePrice: id, TargetPrice: "b"})
			return err
		},
		"deposit source":      func(id string) error { _, err := c.GetDeposit(ctx, customer, id); return err },
		"capture":             func(id string) error { _, err := c.Capture(ctx, id, 1, nil); return err },
		"release":             func(id string) error { return c.Release(ctx, id) },
		"extend hold":         func(id string) error { return c.ExtendHold(ctx, id, now) },
		"entitlement subject": func(id string) error { _, err := c.ListEntitlements(ctx, id, now); return err },
	}
	// Typed identifiers: zero, malformed, or another kind's spelling.
	typed := map[string]func() error{
		"pay invoice":         func() error { _, err := c.PayInvoiceNow(ctx, PayInvoiceNowRequest{}); return err },
		"retry subscription":  func() error { _, err := c.RetrySubscriptionNow(ctx, RetrySubscriptionNowRequest{}); return err },
		"my invoice":          func() error { _, err := c.GetMyInvoice(ctx, uuid.Nil); return err },
		"my subscription":     func() error { _, err := c.GetMySubscription(ctx, SubscriptionID{}); return err },
		"subscription":        func() error { _, err := c.GetSubscription(ctx, SubscriptionID{}); return err },
		"cancel subscription": func() error { return c.CancelSubscription(ctx, SubscriptionID{}, CancelSubscriptionRequest{}) },
		"resume subscription": func() error { return c.ResumeSubscription(ctx, SubscriptionID{}) },
		"payment":             func() error { _, err := c.GetPayment(ctx, PaymentID{}); return err },
		"product":             func() error { _, err := c.Products.Retrieve(ctx, ""); return err },
		"product wrong kind":  func() error { _, err := c.Products.Retrieve(ctx, price.String()); return err },
		"update product":      func() error { _, err := c.Products.Update(ctx, "", &ProductUpdateParams{}); return err },
		"price":               func() error { _, err := c.Prices.Retrieve(ctx, ""); return err },
		"update price":        func() error { _, err := c.Prices.Update(ctx, "", &PriceUpdateParams{}); return err },
		"set price key id":    func() error { _, err := c.Prices.SetKey(ctx, "", "key"); return err },
		"checkout rails":      func() error { _, err := c.ListCheckoutRailOptions(ctx, noPrice); return err },
		"checkout session":    func() error { _, err := c.GetCheckoutSession(ctx, customer, ""); return err },
		"checkout session kind": func() error {
			_, err := c.GetCheckoutSession(ctx, customer, subscription.String())
			return err
		},
		"checkout session holder": func() error {
			_, err := c.GetCheckoutSession(ctx, "", CheckoutSessionID(uuid.New()).String())
			return err
		},
		"confirm checkout": func() error { _, err := c.ConfirmCheckoutSession(ctx, "", ConfirmCheckoutSessionRequest{}); return err },
		"settled payment":  func() error { _, err := c.HasSettledPayment(ctx, customer, noPrice); return err },
		"access check product": func() error {
			_, err := c.ProductAccess.Check(ctx, &ProductAccessCheckParams{CustomerID: customer})
			return err
		},
		"access check both": func() error {
			_, err := c.ProductAccess.Check(ctx, &ProductAccessCheckParams{CustomerID: customer, ProductID: ProductID(uuid.New()).String(), ProductKey: "k"})
			return err
		},
		"access batch wrong kind": func() error {
			_, err := c.ProductAccess.CheckMany(ctx, &ProductAccessCheckManyParams{CustomerID: customer, ProductIDs: []string{price.String()}})
			return err
		},
		"access batch over limit": func() error {
			_, err := c.ProductAccess.CheckMany(ctx, &ProductAccessCheckManyParams{CustomerID: customer, ProductIDs: make([]string, ProductAccessMaxPageSize+1)})
			return err
		},
		"access batch ids and keys": func() error {
			_, err := c.ProductAccess.CheckMany(ctx, &ProductAccessCheckManyParams{CustomerID: customer, ProductIDs: []string{}, ProductKeys: []string{}})
			return err
		},
		"access list over limit": func() error {
			_, err := c.ProductAccess.List(ctx, &ProductAccessListParams{CustomerID: customer, Limit: ProductAccessMaxPageSize + 1})
			return err
		},
		"access nil params": func() error { _, err := c.ProductAccess.List(ctx, nil); return err },
		"subscription payment method": func() error {
			return c.UpdateSubscriptionPaymentMethod(ctx, SubscriptionID{}, UpdateSubscriptionPaymentMethodRequest{PaymentMethodID: method})
		},
		"payment method for subscription": func() error {
			return c.UpdateSubscriptionPaymentMethod(ctx, subscription, UpdateSubscriptionPaymentMethodRequest{})
		},
		"tier preview subscription": func() error {
			_, err := c.PreviewTierChange(ctx, SubscriptionID{}, ChangeTierRequest{PriceID: price.String()})
			return err
		},
		"tier preview price": func() error { _, err := c.PreviewTierChange(ctx, subscription, ChangeTierRequest{}); return err },
		"tier change price":  func() error { _, err := c.ChangeTier(ctx, subscription, "key", ChangeTierRequest{}); return err },
		"delete payment method": func() error {
			_, err := c.DeletePaymentMethod(ctx, customer, PaymentMethodID{})
			return err
		},
		"empty delegations customer": func() error { return c.SetCustomerSpendDelegations(ctx, noCustomer, nil) },
		"entitlement check": func() error {
			_, err := c.HasEntitlement(ctx, customer, "", time.Time{})
			return err
		},
		"entitled customers": func() error { _, err := c.ListCustomersWithEntitlement(ctx, "", time.Time{}); return err },
	}
	// Every customer-scoped operation validates the customer the same way.
	customerScoped := map[string]func(id string) error{
		"payment methods":     func(id string) error { _, err := c.ListPaymentMethods(ctx, id, PageOptions{}); return err },
		"effective tier":      func(id string) error { _, err := c.ResolveEffectiveTier(ctx, id, "group"); return err },
		"invoice profile":     func(id string) error { _, err := c.GetCustomerInvoiceProfile(ctx, id); return err },
		"set invoice profile": func(id string) error { return c.SetCustomerInvoiceProfile(ctx, id, InvoiceProfileDTO{}) },
		"balance":             func(id string) error { _, err := c.Balance(ctx, id); return err },
		"credit account":      func(id string) error { _, err := c.GetCreditAccount(ctx, id, "USD"); return err },
		"usage rollup":        func(id string) error { _, err := c.UsageRollup(ctx, id, "USD", now, now, "day"); return err },
		"trust level":         func(id string) error { _, err := c.GetTrustLevel(ctx, id, "USD"); return err },
		"set credit limit":    func(id string) error { return c.SetCreditLimit(ctx, id, "USD", 1) },
		"credit limit":        func(id string) error { _, err := c.GetCreditLimit(ctx, id, "USD"); return err },
		"access list": func(id string) error {
			_, err := c.ProductAccess.List(ctx, &ProductAccessListParams{CustomerID: id})
			return err
		},
		"billing policy":     func(id string) error { _, err := c.GetCustomerBillingPolicy(ctx, id); return err },
		"set billing policy": func(id string) error { _, err := c.SetCustomerBillingPolicy(ctx, id, nil); return err },
		"set delegation":     func(id string) error { return c.SetCustomerSpendDelegation(ctx, id, SpendDelegationInput{}) },
		"ensure customer":    func(id string) error { _, err := c.EnsureCustomer(ctx, id); return err },
		"deposit customer":   func(id string) error { _, err := c.GetDeposit(ctx, id, "src"); return err },
		"grant customer": func(id string) error {
			_, err := c.GrantEntitlement(ctx, id, GrantEntitlementRequest{Entitlement: "pro"})
			return err
		},
		"revoke customer":       func(id string) error { return c.RevokeEntitlement(ctx, id, "ent") },
		"settled payment buyer": func(id string) error { _, err := c.HasSettledPayment(ctx, id, price.String()); return err },
	}
	uuidCalls := map[string]func() error{
		"merchant invoice":      func() error { _, err := c.GetMerchantInvoice(ctx, uuid.Nil); return err },
		"void invoice":          func() error { _, err := c.VoidInvoice(ctx, uuid.Nil); return err },
		"uncollectible invoice": func() error { _, err := c.MarkInvoiceUncollectible(ctx, uuid.Nil); return err },
		"record invoice payment": func() error {
			_, err := c.RecordInvoicePayment(ctx, uuid.Nil, RecordInvoicePaymentRequest{})
			return err
		},
		"retry invoice":          func() error { _, err := c.RetryInvoiceCollection(ctx, InvoiceCollectionRetryRequest{}); return err },
		"invoice attempts":       func() error { _, _, err := c.ListInvoicePaymentAttempts(ctx, uuid.Nil, 10, 0); return err },
		"cancel migration":       func() error { _, err := c.CancelPlanMigration(ctx, uuid.Nil); return err },
		"acknowledge host event": func() error { return c.AcknowledgeHostEvent(ctx, uuid.Nil) },
		"ensure invoice profile": func() error {
			_, err := c.EnsureCustomerInvoiceProfile(ctx, noCustomer, InvoiceProfileDTO{})
			return err
		},
	}

	requireInvalidParam := func(t *testing.T, name string, err error) {
		t.Helper()
		var status *StatusError
		require.ErrorAs(t, err, &status, name)
		require.Equal(t, ErrorDetails{Type: "invalid_request_error", Code: "invalid_param", Message: status.Message}, status.ErrorDetails, name)
		require.Equal(t, http.StatusBadRequest, status.Status, name)
		require.NotEmpty(t, status.Message, name)
	}
	for name, call := range pathStrings {
		for _, id := range []string{"", "   ", "\t\n", ".", "..", " . "} {
			requireInvalidParam(t, name+" "+id, call(id))
		}
	}
	for name, call := range blankOnly {
		for _, id := range []string{"", "   ", "\t\n"} {
			requireInvalidParam(t, name, call(id))
		}
	}
	for name, call := range customerScoped {
		for _, id := range []string{"", noCustomer, "not-a-uuid", PriceID(uuid.New()).String(), strings.ToUpper("cus_" + uuid.NewString())} {
			requireInvalidParam(t, name+" "+id, call(id))
		}
	}
	for name, call := range typed {
		requireInvalidParam(t, name, call())
	}
	for name, call := range uuidCalls {
		requireInvalidParam(t, name, call())
	}
}
