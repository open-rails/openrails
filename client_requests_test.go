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

	"github.com/open-rails/openrails/billing"
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
	customer := billing.CustomerID(uuid.MustParse("7d5b4a0e-8c3f-4c1e-9b2a-1f0e2d3c4b5a")).String()
	product := billing.ProductID(uuid.New()).String()
	client, seen := recordingRemote(t, map[string]string{
		"/v1/merchant/admissions":                                      `{"items":[{"status":200,"admission":{"allowed":true,"state":"open"},"error":null}]}`,
		"/v1/merchant/customers/" + customer + "/trust-level":          `{"customer_id":"` + customer + `","currency":"USD","trust_level":"gold"}`,
		"/v1/merchant/customers/" + customer + "/credit-grants":        `{"data":[{"amount":"1"}],"next_cursor":null}`,
		"/v1/merchant/customers/" + customer + "/product-access":       `{"customer_id":"` + customer + `","product_id":"` + product + `","has_access":true,"data":[],"has_more":true,"next_cursor":"next"}`,
		"/v1/merchant/customers/" + customer + "/product-access/check": `{"access":{"` + product + `":true}}`,
	})
	key := "operation-key"
	who := billing.CheckoutCustomerIdentity{ID: billing.CustomerID(uuid.MustParse(customer))}
	expires := time.Now().Add(time.Hour)
	window := []billing.BudgetWindow{{Key: "month", WindowSeconds: 2592000, Limit: 42, Currency: "USD"}}
	typedCustomer := billing.CustomerID(uuid.MustParse(customer))
	cases := []struct {
		name   string
		call   func() error
		method string
		path   string
		query  string
		check  func(t *testing.T, body map[string]any)
	}{
		{"checkout attempt", func() error {
			_, err := client.CreateCheckoutAttempt(t.Context(), billing.CreateCheckoutAttemptRequest{Customer: who, PriceID: billing.PriceID(uuid.New()), IdempotencyKey: key})
			return err
		}, http.MethodPost, "/v1/merchant/checkout-attempts", "", func(t *testing.T, b map[string]any) {
			require.Contains(t, b, "price_id")
			require.NotContains(t, b, "subscription_id")
			require.NotContains(t, b, "mode", "the price selects the operation")
		}},
		{"checkout session", func() error {
			_, err := client.CreateCheckoutSession(t.Context(), billing.CreateCheckoutSessionRequest{Customer: who, PriceKey: "pro-monthly"})
			return err
		}, http.MethodPost, "/v1/merchant/checkout-sessions", "", func(t *testing.T, b map[string]any) {
			require.Equal(t, "pro-monthly", b["price_key"])
			require.Equal(t, who.ID.String(), b["customer"].(map[string]any)["id"])
		}},
		{"checkout config for a price", func() error {
			_, err := client.GetCheckoutConfig(t.Context(), billing.CheckoutConfigQuery{PriceKey: "pro-monthly"})
			return err
		}, http.MethodGet, "/v1/merchant/checkout-config", "price_key=pro-monthly", nil},
		{"settings carry named policies and tier bindings", func() error {
			return client.SetMerchantSettings(t.Context(), billing.MerchantSettings{
				BillingPolicies:       []billing.BillingPolicyInput{{Name: "api_line", Kind: "outstanding_cap", OutstandingCapAmount: 200_000_000}},
				BillingPolicyBindings: []billing.BillingPolicyBindingInput{{PolicyName: "api_line", Tier: "gold"}},
			})
		}, http.MethodPut, "/v1/merchant/settings", "", func(t *testing.T, b map[string]any) {
			require.Equal(t, "api_line", b["billing_policies"].([]any)[0].(map[string]any)["name"])
			binding := b["billing_policy_bindings"].([]any)[0].(map[string]any)
			require.Equal(t, "api_line", binding["policy"])
			require.Equal(t, "gold", binding["tier"])
		}},
		{"admission carries trust level and prospective rate", func() error {
			verdicts, err := client.Admit(t.Context(), []billing.AdmitParams{{CustomerID: typedCustomer, TrustLevel: "gold", EstimatedAmount: 1, ExpiresAt: &expires, RequestID: "req_1", AccrualRateDeltaPerHour: 42}})
			if err == nil && !verdicts[0].Allowed() {
				err = errors.New("verdict not decoded")
			}
			return err
		}, http.MethodPost, "/v1/merchant/admissions", "", func(t *testing.T, b map[string]any) {
			item := b["items"].([]any)[0].(map[string]any)
			require.Equal(t, "gold", item["trust_level"])
			require.Equal(t, "42", item["accrual_rate_delta_per_hour"])
		}},
		{"trust level read", func() error {
			level, err := client.GetTrustLevel(t.Context(), typedCustomer, " USD ")
			if err == nil && level.TrustLevel != "gold" {
				err = errors.New("trust level not decoded: " + level.TrustLevel)
			}
			return err
		}, http.MethodGet, "/v1/merchant/customers/" + customer + "/trust-level", "currency=USD", nil},
		{"credit grant source ids are opaque query values", func() error {
			page, err := client.ListCreditGrants(t.Context(), typedCustomer, billing.CreditGrantListParams{SourceID: "../source/receipt?part=1&currency=JPY"})
			if err == nil && page.Items[0].Amount != 1 {
				err = errors.New("grant amount not decoded")
			}
			return err
		}, http.MethodGet, "/v1/merchant/customers/" + customer + "/credit-grants", "source_id=..%2Fsource%2Freceipt%3Fpart%3D1%26currency%3DJPY", nil},
		{"delegation replace", func() error {
			_, err := client.SetSpendDelegations(t.Context(), typedCustomer, []billing.SpendDelegation{{Scope: billing.SpendDelegationInvoker, ScopeKey: "invoker-1", Windows: window}})
			return err
		}, http.MethodPut, "/v1/merchant/customers/" + customer + "/spend-delegations", "", func(t *testing.T, b map[string]any) {
			require.Len(t, b["delegations"], 1)
		}},
		{"delegation set at its address", func() error {
			_, err := client.SetSpendDelegation(t.Context(), typedCustomer, billing.SpendDelegation{Scope: billing.SpendDelegationInvoker, ScopeKey: "issuer:subject", Windows: window})
			return err
		}, http.MethodPut, "/v1/merchant/customers/" + customer + "/spend-delegations/invoker/issuer:subject", "", func(t *testing.T, b map[string]any) {
			require.NotContains(t, b, "scope_key", "the address carries scope and key")
			require.Len(t, b["windows"], 1)
		}},
		{"delegation delete escapes one segment per key", func() error {
			return client.DeleteSpendDelegation(t.Context(), typedCustomer, " invoker ", "a/b:c")
		}, http.MethodDelete, "/v1/merchant/customers/" + customer + "/spend-delegations/invoker/a%2Fb:c", "", nil},
		{"access check by id", func() error {
			got, err := client.ProductAccess.Check(t.Context(), &billing.ProductAccessCheckParams{CustomerID: customer, ProductID: product})
			if err == nil && !got.HasAccess {
				err = errors.New("access not decoded")
			}
			return err
		}, http.MethodGet, "/v1/merchant/customers/" + customer + "/product-access", "product_id=" + product, nil},
		{"access check by key", func() error {
			_, err := client.ProductAccess.Check(t.Context(), &billing.ProductAccessCheckParams{CustomerID: customer, ProductKey: "pro plan&x"})
			return err
		}, http.MethodGet, "/v1/merchant/customers/" + customer + "/product-access", "product_key=pro+plan%26x", nil},
		{"access batch keeps duplicates", func() error {
			got, err := client.ProductAccess.CheckMany(t.Context(), &billing.ProductAccessCheckManyParams{CustomerID: customer, ProductIDs: []string{product, product}})
			if err == nil && !got[product] {
				err = errors.New("batch access not decoded")
			}
			return err
		}, http.MethodPost, "/v1/merchant/customers/" + customer + "/product-access/check", "", func(t *testing.T, b map[string]any) {
			require.Equal(t, []any{product, product}, b["product_ids"])
		}},
		{"access list pages", func() error {
			page, err := client.ProductAccess.List(t.Context(), &billing.ProductAccessListParams{CustomerID: customer, Limit: 7, Cursor: "cursor"})
			if err == nil && (!page.HasMore || page.NextCursor != "next") {
				err = errors.New("page not decoded")
			}
			return err
		}, http.MethodGet, "/v1/merchant/customers/" + customer + "/product-access", "cursor=cursor&limit=7", nil},
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
	empty, err := client.ProductAccess.CheckMany(t.Context(), &billing.ProductAccessCheckManyParams{CustomerID: customer, ProductIDs: []string{}})
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
	customer, price, subscription, method := billing.CustomerID(uuid.New()).String(), billing.PriceID(uuid.New()), billing.SubscriptionID(uuid.New()), billing.PaymentMethodID(uuid.New())
	noCustomer := billing.CustomerID{}.String()

	// Free-form host strings: blank and dot segments are refused.
	pathStrings := map[string]func(id string) error{
		"product key":   func(id string) error { _, err := c.GetProductByKey(ctx, id); return err },
		"price key":     func(id string) error { _, err := c.GetPriceByKey(ctx, id); return err },
		"price history": func(id string) error { _, err := c.ListPriceKeyHistory(ctx, id, billing.PageRequest{}); return err },
		"meter":         func(id string) error { _, err := c.GetMeter(ctx, id); return err },
		"set meter":     func(id string) error { _, err := c.SetMeter(ctx, id, billing.SetMeterParams{}); return err },
		"set rate card": func(id string) error {
			_, err := c.SetMeterRateCard(ctx, id, billing.SetMeterRateCardParams{})
			return err
		},
		"delete rate card": func(id string) error { return c.DeleteMeterRateCard(ctx, id) },
		"rate override meter": func(id string) error {
			_, err := c.SetRateOverride(ctx, billing.CustomerID(uuid.New()), id, billing.SetRateOverrideParams{})
			return err
		},
		"delete delegation scope": func(id string) error {
			return c.DeleteSpendDelegation(ctx, billing.CustomerID(uuid.New()), billing.SpendDelegationScope(id), "key")
		},
		"delete delegation key": func(id string) error {
			return c.DeleteSpendDelegation(ctx, billing.CustomerID(uuid.New()), billing.SpendDelegationInvoker, id)
		},
		"grant entitlement": func(id string) error {
			_, err := c.GrantEntitlement(ctx, customer, billing.GrantEntitlementRequest{Entitlement: id})
			return err
		},
		"revoke entitlement":      func(id string) error { return c.RevokeEntitlement(ctx, customer, id) },
		"operation authorization": func(id string) error { _, err := c.GetOperationAuthorization(ctx, id); return err },
		"billing qualification":   func(id string) error { _, err := c.GetProviderBillingQualification(ctx, id); return err },
	}
	// Opaque host strings (request ids, deposit keys, migration prices): only blankness is refused.
	blankOnly := map[string]func(id string) error{
		"preview migration source": func(id string) error {
			_, err := c.PreviewPlanMigration(ctx, billing.PlanMigrationRequest{SourcePrice: id, TargetPrice: "b"})
			return err
		},
		"preview migration target": func(id string) error {
			_, err := c.PreviewPlanMigration(ctx, billing.PlanMigrationRequest{SourcePrice: "a", TargetPrice: id})
			return err
		},
		"create migration source": func(id string) error {
			_, err := c.CreatePlanMigration(ctx, billing.PlanMigrationRequest{SourcePrice: id, TargetPrice: "b"})
			return err
		},
		"capture": func(id string) error {
			_, err := c.CaptureAdmission(ctx, id, billing.CaptureParams{Amount: 1})
			return err
		},
		"release": func(id string) error { _, err := c.ReleaseAdmission(ctx, id); return err },
		"extend hold": func(id string) error {
			_, err := c.ExtendAdmission(ctx, id, billing.ExtendAdmissionParams{ExpiresAt: now})
			return err
		},
		"admission":           func(id string) error { _, err := c.GetAdmission(ctx, id); return err },
		"entitlement subject": func(id string) error { _, err := c.ListEntitlements(ctx, id, now); return err },
	}
	// Typed identifiers: zero, malformed, or another kind's spelling.
	typed := map[string]func() error{
		"retry subscription": func() error { _, err := c.RetrySubscriptionNow(ctx, billing.RetrySubscriptionNowRequest{}); return err },
		"invoice":            func() error { _, err := c.GetInvoice(ctx, billing.InvoiceID{}); return err },
		"void invoice":       func() error { _, err := c.VoidInvoice(ctx, billing.InvoiceID{}); return err },
		"uncollectible invoice": func() error {
			_, err := c.MarkInvoiceUncollectible(ctx, billing.InvoiceID{})
			return err
		},
		"invoice payment": func() error {
			_, err := c.CreateInvoicePayment(ctx, billing.InvoiceID{}, billing.CreateInvoicePaymentParams{})
			return err
		},
		"invoice payments": func() error {
			_, err := c.ListInvoicePayments(ctx, billing.InvoiceID{}, billing.PageRequest{})
			return err
		},
		"retry invoice": func() error {
			_, err := c.RetryInvoiceCollection(ctx, billing.InvoiceID{}, billing.RetryInvoiceCollectionParams{IdempotencyKey: "k"})
			return err
		},
		"payment methods customer": func() error {
			_, err := c.ListPaymentMethods(ctx, billing.CustomerID{}, billing.PageRequest{})
			return err
		},
		"invoice profile customer": func() error { _, err := c.GetCustomerInvoiceProfile(ctx, billing.CustomerID{}); return err },
		"set invoice profile customer": func() error {
			_, err := c.SetCustomerInvoiceProfile(ctx, billing.CustomerID{}, billing.SetInvoiceProfileParams{IfAbsent: true})
			return err
		},
		"settled payment customer": func() error {
			_, err := c.GetPaymentSettlementStatus(ctx, billing.CustomerID{}, price)
			return err
		},
		"settled payment price": func() error {
			_, err := c.GetPaymentSettlementStatus(ctx, billing.CustomerID(uuid.New()), billing.PriceID{})
			return err
		},
		"off-channel payment": func() error {
			_, err := c.CreateOffChannelPayment(ctx, billing.CustomerID(uuid.New()), billing.CreateOffChannelPaymentParams{})
			return err
		},
		"my subscription": func() error { _, err := c.GetMySubscription(ctx, billing.SubscriptionID{}); return err },
		"subscription":    func() error { _, err := c.GetSubscription(ctx, billing.SubscriptionID{}); return err },
		"cancel subscription": func() error {
			return c.CancelSubscription(ctx, billing.SubscriptionID{}, billing.CancelSubscriptionRequest{})
		},
		"resume subscription": func() error { return c.ResumeSubscription(ctx, billing.SubscriptionID{}) },
		"payment":             func() error { _, err := c.GetPayment(ctx, billing.PaymentID{}); return err },
		"product":             func() error { _, err := c.GetProduct(ctx, billing.ProductID{}); return err },
		"update product": func() error {
			_, err := c.UpdateProduct(ctx, billing.ProductID{}, billing.UpdateProductParams{})
			return err
		},
		"price":        func() error { _, err := c.GetPrice(ctx, billing.PriceID{}, billing.GetPriceParams{}); return err },
		"update price": func() error { _, err := c.UpdatePrice(ctx, billing.PriceID{}, billing.UpdatePriceParams{}); return err },
		"catalog":      func() error { _, err := c.GetCatalog(ctx, billing.CatalogID{}); return err },
		"rate overrides": func() error {
			_, err := c.ListRateOverrides(ctx, billing.CustomerID{}, billing.PageRequest{})
			return err
		},
		"checkout options": func() error {
			_, err := c.GetCheckoutConfig(ctx, billing.CheckoutConfigQuery{PriceID: billing.PriceID(uuid.New()), PriceKey: "k"})
			return err
		},
		"checkout attempt": func() error { _, err := c.GetCheckoutAttempt(ctx, billing.CheckoutAttemptID{}); return err },
		"confirm checkout": func() error {
			_, err := c.ConfirmCheckoutAttempt(ctx, billing.CheckoutAttemptID{}, billing.ConfirmCheckoutAttemptRequest{})
			return err
		},
		"checkout customer": func() error {
			_, err := c.CreateCheckoutAttempt(ctx, billing.CreateCheckoutAttemptRequest{PriceKey: "k", IdempotencyKey: "k"})
			return err
		},
		"checkout price": func() error {
			_, err := c.CreateCheckoutSession(ctx, billing.CreateCheckoutSessionRequest{Customer: billing.CheckoutCustomerIdentity{ID: billing.CustomerID(uuid.New())}})
			return err
		},
		"access check product": func() error {
			_, err := c.ProductAccess.Check(ctx, &billing.ProductAccessCheckParams{CustomerID: customer})
			return err
		},
		"access check both": func() error {
			_, err := c.ProductAccess.Check(ctx, &billing.ProductAccessCheckParams{CustomerID: customer, ProductID: billing.ProductID(uuid.New()).String(), ProductKey: "k"})
			return err
		},
		"access batch wrong kind": func() error {
			_, err := c.ProductAccess.CheckMany(ctx, &billing.ProductAccessCheckManyParams{CustomerID: customer, ProductIDs: []string{price.String()}})
			return err
		},
		"access batch over limit": func() error {
			_, err := c.ProductAccess.CheckMany(ctx, &billing.ProductAccessCheckManyParams{CustomerID: customer, ProductIDs: make([]string, billing.ProductAccessMaxPageSize+1)})
			return err
		},
		"access batch ids and keys": func() error {
			_, err := c.ProductAccess.CheckMany(ctx, &billing.ProductAccessCheckManyParams{CustomerID: customer, ProductIDs: []string{}, ProductKeys: []string{}})
			return err
		},
		"access list over limit": func() error {
			_, err := c.ProductAccess.List(ctx, &billing.ProductAccessListParams{CustomerID: customer, Limit: billing.ProductAccessMaxPageSize + 1})
			return err
		},
		"access nil params": func() error { _, err := c.ProductAccess.List(ctx, nil); return err },
		"subscription payment method": func() error {
			return c.UpdateSubscriptionPaymentMethod(ctx, billing.SubscriptionID{}, billing.UpdateSubscriptionPaymentMethodRequest{PaymentMethodID: method})
		},
		"payment method for subscription": func() error {
			return c.UpdateSubscriptionPaymentMethod(ctx, subscription, billing.UpdateSubscriptionPaymentMethodRequest{})
		},
		"tier preview subscription": func() error {
			_, err := c.PreviewTierChange(ctx, billing.SubscriptionID{}, billing.ChangeTierRequest{PriceID: price.String()})
			return err
		},
		"tier preview price": func() error {
			_, err := c.PreviewTierChange(ctx, subscription, billing.ChangeTierRequest{})
			return err
		},
		"tier change price": func() error {
			_, err := c.ChangeTier(ctx, subscription, "key", billing.ChangeTierRequest{})
			return err
		},
		"delete payment method": func() error {
			_, err := c.DeletePaymentMethod(ctx, billing.CustomerID(uuid.New()), billing.PaymentMethodID{})
			return err
		},
		"empty delegations customer": func() error { _, err := c.SetSpendDelegations(ctx, billing.CustomerID{}, nil); return err },
		"entitlement check": func() error {
			_, err := c.HasEntitlement(ctx, customer, "", time.Time{})
			return err
		},
		"entitled customers": func() error { _, err := c.ListCustomersWithEntitlement(ctx, "", time.Time{}); return err },
	}
	// Every customer-scoped operation validates the customer the same way.
	customerScoped := map[string]func(id string) error{
		"effective tier": func(id string) error { _, err := c.ResolveEffectiveTier(ctx, id, "group"); return err },
		"access list": func(id string) error {
			_, err := c.ProductAccess.List(ctx, &billing.ProductAccessListParams{CustomerID: id})
			return err
		},

		"grant customer": func(id string) error {
			_, err := c.GrantEntitlement(ctx, id, billing.GrantEntitlementRequest{Entitlement: "pro"})
			return err
		},
		"revoke customer": func(id string) error { return c.RevokeEntitlement(ctx, id, "ent") },
	}
	// Typed customer ids: the zero id names nobody.
	zero := billing.CustomerID{}
	typedCustomerScoped := map[string]func() error{
		"balance":          func() error { _, err := c.GetBalance(ctx, zero, "USD"); return err },
		"usage":            func() error { _, err := c.GetUsage(ctx, zero, billing.UsageParams{Currency: "USD"}); return err },
		"trust level":      func() error { _, err := c.GetTrustLevel(ctx, zero, "USD"); return err },
		"set trust level":  func() error { _, err := c.SetTrustLevel(ctx, zero, billing.TrustLevelParams{}); return err },
		"credit limit":     func() error { _, err := c.GetCreditLimit(ctx, zero, "USD"); return err },
		"set credit limit": func() error { _, err := c.SetCreditLimit(ctx, zero, billing.CreditLimitParams{}); return err },
		"credit grants":    func() error { _, err := c.ListCreditGrants(ctx, zero, billing.CreditGrantListParams{}); return err },
		"create credit":    func() error { _, err := c.CreateCreditGrant(ctx, zero, billing.CreditGrantParams{}); return err },
		"transactions": func() error {
			_, err := c.ListCreditTransactions(ctx, zero, billing.CreditTransactionListParams{})
			return err
		},
		"billing policy": func() error { _, err := c.GetCustomerBillingPolicy(ctx, zero); return err },
		"set billing policy": func() error {
			_, err := c.SetCustomerBillingPolicy(ctx, zero, billing.CustomerBillingPolicyParams{})
			return err
		},
		"delegations": func() error { _, err := c.ListSpendDelegations(ctx, zero); return err },
		"set delegation": func() error {
			_, err := c.SetSpendDelegation(ctx, zero, billing.SpendDelegation{Scope: billing.SpendDelegationInvoker, ScopeKey: "k"})
			return err
		},
		"customer":        func() error { _, err := c.GetCustomer(ctx, zero); return err },
		"ensure customer": func() error { _, err := c.EnsureCustomer(ctx, zero, billing.CustomerParams{}); return err },
		"billing profile": func() error { _, err := c.GetCustomerBillingProfile(ctx, zero); return err },
		"delinquency":     func() error { _, err := c.ListCustomerDelinquency(ctx, zero); return err },
		"credit grant": func() error {
			_, err := c.GetCreditGrant(ctx, billing.CustomerID(uuid.New()), billing.CreditGrantID{})
			return err
		},
		"revoke credit": func() error {
			_, err := c.RevokeCreditGrant(ctx, billing.CustomerID(uuid.New()), billing.CreditGrantID{}, billing.RevokeCreditGrantParams{})
			return err
		},
	}
	uuidCalls := map[string]func() error{
		"cancel migration":       func() error { _, err := c.CancelPlanMigration(ctx, uuid.Nil); return err },
		"acknowledge host event": func() error { return c.AcknowledgeHostEvent(ctx, uuid.Nil) },
	}

	requireInvalidParam := func(t *testing.T, name string, err error) {
		t.Helper()
		var status *billing.StatusError
		require.ErrorAs(t, err, &status, name)
		require.Equal(t, billing.ErrorDetails{Type: "invalid_request_error", Code: "invalid_param", Message: status.Message}, status.ErrorDetails, name)
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
		for _, id := range []string{"", noCustomer, "not-a-uuid", billing.PriceID(uuid.New()).String(), strings.ToUpper("cus_" + uuid.NewString())} {
			requireInvalidParam(t, name+" "+id, call(id))
		}
	}
	for name, call := range typed {
		requireInvalidParam(t, name, call())
	}
	for name, call := range typedCustomerScoped {
		requireInvalidParam(t, name, call())
	}
	for name, call := range uuidCalls {
		requireInvalidParam(t, name, call())
	}
}
