package openrails

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
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
	customerID := billing.CustomerID(uuid.MustParse("7d5b4a0e-8c3f-4c1e-9b2a-1f0e2d3c4b5a"))
	customer := customerID.String()
	product := billing.ProductID(uuid.New()).String()
	client, seen := recordingRemote(t, map[string]string{
		"/v1/merchant/admissions":                                      `{"items":[{"status":200,"admission":{"allowed":true,"state":"open"},"error":null}]}`,
		"/v1/merchant/customers/settings":                              `{"data":[{"customer_id":"` + customer + `","credit_limits":[],"trust_levels":[{"currency":"USD","trust_level":"gold"}],"billing_policy":null,"invoice_profile":null}],"next_cursor":null}`,
		"/v1/merchant/customers/" + customer + "/credit-grants":        `{"data":[{"amount":"1"}],"next_cursor":null}`,
		"/v1/merchant/customers/" + customer + "/product-access":       `{"data":[],"next_cursor":"next"}`,
		"/v1/merchant/customers/" + customer + "/product-access/check": `{"access":{"` + product + `":true}}`,
		"/v1/merchant/customers/" + customer + "/entitlements/check":   `{"entitlements":{"pro":true,"team":false}}`,
	})
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
		{"checkout session", func() error {
			_, err := client.CreateCheckoutSession(t.Context(), billing.CreateCheckoutSessionParams{Customer: who, ProductKey: "pro", PriceKey: "pro-monthly"})
			return err
		}, http.MethodPost, "/v1/merchant/checkout-sessions", "", func(t *testing.T, b map[string]any) {
			require.Equal(t, "pro-monthly", b["price_key"])
			require.Equal(t, who.ID.String(), b["customer"].(map[string]any)["id"])
		}},
		{"checkout config for a price", func() error {
			_, err := client.GetCheckoutConfig(t.Context(), billing.GetCheckoutConfigParams{ProductKey: "pro", PriceKey: "pro-monthly"})
			return err
		}, http.MethodGet, "/v1/merchant/checkout-config", "price_key=pro-monthly&product_key=pro", nil},
		{"settings carry named policies and tier bindings", func() error {
			revision := "r1"
			_, err := client.ApplyMerchantConfiguration(t.Context(), billing.ApplyMerchantConfigurationParams{ApplicationID: "a1", ExpectedRevision: &revision, Settings: &billing.MerchantSettings{
				BillingPolicies:       []billing.BillingPolicy{{Name: "api_line", Kind: "outstanding_cap", OutstandingCapAmount: 200_000_000}},
				BillingPolicyBindings: []billing.BillingPolicyBinding{{PolicyName: "api_line", Tier: "gold"}},
			}})
			return err
		}, http.MethodPost, "/v1/merchant/configuration/applications", "", func(t *testing.T, b map[string]any) {
			settings := b["settings"].(map[string]any)
			require.Equal(t, "api_line", settings["billing_policies"].([]any)[0].(map[string]any)["name"])
			binding := settings["billing_policy_bindings"].([]any)[0].(map[string]any)
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
		{"settings read names each customer", func() error {
			page, err := client.ListCustomerSettings(t.Context(), billing.CustomerSettingsListParams{IDs: []billing.CustomerID{typedCustomer, typedCustomer}})
			if err == nil && page.Items[0].TrustLevels[0].TrustLevel != "gold" {
				err = errors.New("settings not decoded")
			}
			return err
		}, http.MethodGet, "/v1/merchant/customers/settings", "ids=" + customer + "%2C" + customer, nil},
		{"settings write sends only the named fields", func() error {
			_, err := client.UpdateCustomerSettings(t.Context(), []billing.UpdateCustomerSettingsParams{{CustomerID: typedCustomer, BillingPolicy: catalog.Null[string]()}})
			return err
		}, http.MethodPatch, "/v1/merchant/customers/settings", "", func(t *testing.T, b map[string]any) {
			require.Equal(t, map[string]any{"items": []any{map[string]any{"customer_id": customer, "billing_policy": nil}}}, b)
		}},
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
		{"access check keeps duplicates", func() error {
			id, _ := billing.ParseProductID(product)
			got, err := client.CheckProductAccess(t.Context(), customerID, billing.CheckProductAccessParams{ProductIDs: []billing.ProductID{id, id}})
			if err == nil && !got[product] {
				err = errors.New("access not decoded")
			}
			return err
		}, http.MethodPost, "/v1/merchant/customers/" + customer + "/product-access/check", "", func(t *testing.T, b map[string]any) {
			require.Equal(t, []any{product, product}, b["product_ids"])
		}},
		{"access check by key", func() error {
			_, err := client.CheckProductAccess(t.Context(), customerID, billing.CheckProductAccessParams{ProductKeys: []string{"pro plan&x"}})
			return err
		}, http.MethodPost, "/v1/merchant/customers/" + customer + "/product-access/check", "", func(t *testing.T, b map[string]any) {
			require.Equal(t, []any{"pro plan&x"}, b["product_keys"])
		}},
		{"entitlement check", func() error {
			got, err := client.CheckEntitlements(t.Context(), customerID, billing.CheckEntitlementsParams{Entitlements: []string{"pro", "team"}})
			if err == nil && (!got.Entitlements["pro"] || got.Entitlements["team"] || len(got.Entitlements) != 2 || got.Held == nil) {
				err = errors.New("check not decoded")
			}
			return err
		}, http.MethodPost, "/v1/merchant/customers/" + customer + "/entitlements/check", "", func(t *testing.T, b map[string]any) {
			require.Equal(t, map[string]any{"entitlements": []any{"pro", "team"}}, b, "zero At, prefixes and limit are omitted")
		}},
		{"entitlement check prefixes only", func() error {
			_, err := client.CheckEntitlements(t.Context(), customerID, billing.CheckEntitlementsParams{Prefixes: []string{"content:t:"}, PrefixLimit: 5})
			return err
		}, http.MethodPost, "/v1/merchant/customers/" + customer + "/entitlements/check", "", func(t *testing.T, b map[string]any) {
			require.Equal(t, map[string]any{"entitlements": []any{}, "prefixes": []any{"content:t:"}, "prefix_limit": float64(5)}, b, "no keys send a list, not null")
		}},
		{"access list pages", func() error {
			page, err := client.ListProductAccess(t.Context(), customerID, billing.ProductAccessListParams{PageRequest: billing.PageRequest{Limit: 7, Cursor: "cursor"}, LiveOnly: true})
			if err == nil && page.Next != "next" {
				err = errors.New("page not decoded")
			}
			return err
		}, http.MethodGet, "/v1/merchant/customers/" + customer + "/product-access", "cursor=cursor&limit=7&live=true", nil},
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
	customerID := billing.CustomerID(uuid.New())
	price, subscription, method := billing.PriceID(uuid.New()), billing.SubscriptionID(uuid.New()), billing.PaymentMethodID(uuid.New())

	// Free-form host strings: blank and dot segments are refused.
	pathStrings := map[string]func(id string) error{
		"product key": func(id string) error { _, err := c.GetProductByKey(ctx, id); return err },
		"price key":   func(id string) error { _, err := c.GetPriceByKey(ctx, "product", id); return err },
		"price history": func(id string) error {
			_, err := c.ListPriceKeyHistory(ctx, "product", id, billing.PageRequest{})
			return err
		},
		"meter":     func(id string) error { _, err := c.GetMeter(ctx, id); return err },
		"set meter": func(id string) error { _, err := c.SetMeter(ctx, id, billing.SetMeterParams{}); return err },
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
		"operation authorization": func(id string) error { _, err := c.GetOperationAuthorization(ctx, id); return err },
		"billing qualification":   func(id string) error { _, err := c.GetProviderBillingQualification(ctx, id); return err },
	}
	// Opaque host strings (request ids, deposit keys, migration prices): only blankness is refused.
	blankOnly := map[string]func(id string) error{
		"preview migration source": func(id string) error {
			_, err := c.PreviewPlanMigration(ctx, billing.CreatePlanMigrationParams{SourcePrice: id, TargetPrice: "b"})
			return err
		},
		"preview migration target": func(id string) error {
			_, err := c.PreviewPlanMigration(ctx, billing.CreatePlanMigrationParams{SourcePrice: "a", TargetPrice: id})
			return err
		},
		"create migration source": func(id string) error {
			_, err := c.CreatePlanMigration(ctx, billing.CreatePlanMigrationParams{SourcePrice: id, TargetPrice: "b"})
			return err
		},
		"capture": func(id string) error {
			_, err := c.CaptureAdmission(ctx, id, billing.CaptureAdmissionParams{Amount: 1})
			return err
		},
		"release": func(id string) error { _, err := c.ReleaseAdmission(ctx, id); return err },
		"extend hold": func(id string) error {
			_, err := c.ExtendAdmission(ctx, id, billing.ExtendAdmissionParams{ExpiresAt: now})
			return err
		},
		"admission": func(id string) error { _, err := c.GetAdmission(ctx, id); return err },
		"entitlement check key": func(id string) error {
			_, err := c.CheckEntitlements(ctx, customerID, billing.CheckEntitlementsParams{Entitlements: []string{"pro", id}, At: now})
			return err
		},
		"entitled customers": func(id string) error {
			_, err := c.ListEntitlementCustomers(ctx, id, billing.EntitlementCustomerListParams{})
			return err
		},
	}
	// Typed identifiers: zero, malformed, or another kind's spelling.
	typed := map[string]func() error{
		"invoice":      func() error { _, err := c.GetInvoice(ctx, billing.InvoiceID{}); return err },
		"void invoice": func() error { _, err := c.VoidInvoice(ctx, billing.InvoiceID{}); return err },
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
		"settings customer": func() error {
			_, err := c.ListCustomerSettings(ctx, billing.CustomerSettingsListParams{IDs: []billing.CustomerID{{}}})
			return err
		},
		"empty settings ids": func() error {
			_, err := c.ListCustomerSettings(ctx, billing.CustomerSettingsListParams{IDs: []billing.CustomerID{}})
			return err
		},
		"settings update customer": func() error {
			_, err := c.UpdateCustomerSettings(ctx, []billing.UpdateCustomerSettingsParams{{}})
			return err
		},
		"empty settings update": func() error { _, err := c.UpdateCustomerSettings(ctx, nil); return err },
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
		"subscription": func() error { _, err := c.GetSubscription(ctx, billing.SubscriptionID{}); return err },
		"cancel subscription": func() error {
			_, err := c.CancelSubscription(ctx, billing.SubscriptionID{}, billing.CancelSubscriptionParams{})
			return err
		},
		"resume subscription": func() error { _, err := c.ResumeSubscription(ctx, billing.SubscriptionID{}); return err },
		"payment":             func() error { _, err := c.GetPayment(ctx, billing.PaymentID{}); return err },
		"product":             func() error { _, err := c.GetProduct(ctx, billing.ProductID{}); return err },
		"update product": func() error {
			_, err := c.UpdateProduct(ctx, billing.ProductID{}, billing.UpdateProductParams{})
			return err
		},
		"price":        func() error { _, err := c.GetPrice(ctx, billing.PriceID{}, billing.GetPriceParams{}); return err },
		"update price": func() error { _, err := c.UpdatePrice(ctx, billing.PriceID{}, billing.UpdatePriceParams{}); return err },
		"rate overrides": func() error {
			_, err := c.ListRateOverrides(ctx, billing.CustomerID{}, billing.PageRequest{})
			return err
		},
		"checkout options": func() error {
			_, err := c.GetCheckoutConfig(ctx, billing.GetCheckoutConfigParams{PriceID: billing.PriceID(uuid.New()), PriceKey: "k"})
			return err
		},
		"checkout customer": func() error {
			_, err := c.CreateCheckoutSession(ctx, billing.CreateCheckoutSessionParams{PriceKey: "k"})
			return err
		},
		"checkout price": func() error {
			_, err := c.CreateCheckoutSession(ctx, billing.CreateCheckoutSessionParams{Customer: billing.CheckoutCustomerIdentity{ID: billing.CustomerID(uuid.New())}})
			return err
		},
		"access check customer": func() error {
			_, err := c.CheckProductAccess(ctx, billing.CustomerID{}, billing.CheckProductAccessParams{ProductKeys: []string{"k"}})
			return err
		},
		"access check both": func() error {
			_, err := c.CheckProductAccess(ctx, customerID, billing.CheckProductAccessParams{ProductIDs: []billing.ProductID{}, ProductKeys: []string{}})
			return err
		},
		"access check neither": func() error {
			_, err := c.CheckProductAccess(ctx, customerID, billing.CheckProductAccessParams{})
			return err
		},
		"access list customer": func() error {
			_, err := c.ListProductAccess(ctx, billing.CustomerID{}, billing.ProductAccessListParams{})
			return err
		},
		"create access product": func() error {
			_, err := c.CreateProductAccess(ctx, billing.CreateProductAccessBatchParams{Items: []billing.CreateProductAccessParams{{CustomerID: customerID}}})
			return err
		},
		"create access empty": func() error {
			_, err := c.CreateProductAccess(ctx, billing.CreateProductAccessBatchParams{})
			return err
		},
		"delete access id": func() error { return c.DeleteProductAccess(ctx, customerID, billing.ProductAccessID{}) },
		"list entitlements customer": func() error {
			_, err := c.ListCustomerEntitlements(ctx, billing.CustomerID{}, billing.CustomerEntitlementListParams{})
			return err
		},
		"create access customer": func() error {
			_, err := c.CreateProductAccess(ctx, billing.CreateProductAccessBatchParams{Items: []billing.CreateProductAccessParams{{ProductID: billing.ProductID(uuid.New())}}})
			return err
		},
		"record usage empty": func() error {
			_, err := c.RecordUsage(ctx, nil)
			return err
		},
		"record usage over bound": func() error {
			_, err := c.RecordUsage(ctx, make([]billing.RecordUsageParams, billing.MaxUsageBatchItems+1))
			return err
		},
		"customers empty": func() error {
			_, err := c.GetCustomers(ctx, nil)
			return err
		},
		"customers over bound": func() error {
			_, err := c.GetCustomers(ctx, make([]billing.CustomerID, billing.MaxCustomerLookup+1))
			return err
		},
		"ensure customers empty": func() error {
			_, err := c.EnsureCustomers(ctx, nil)
			return err
		},
		"notifications empty": func() error {
			_, err := c.MarkNotificationsRead(ctx, nil)
			return err
		},
		"host events empty": func() error {
			_, err := c.AcknowledgeHostEvents(ctx, nil)
			return err
		},
		"entitlement check customer": func() error {
			_, err := c.CheckEntitlements(ctx, billing.CustomerID{}, billing.CheckEntitlementsParams{Entitlements: []string{"pro"}})
			return err
		},
		"entitlement check over bound": func() error {
			_, err := c.CheckEntitlements(ctx, customerID, billing.CheckEntitlementsParams{Entitlements: make([]string, billing.MaxEntitlementChecks+1)})
			return err
		},
		"entitlement check empty": func() error {
			_, err := c.CheckEntitlements(ctx, customerID, billing.CheckEntitlementsParams{Entitlements: []string{}})
			return err
		},
		"entitlement check prefixes over bound": func() error {
			_, err := c.CheckEntitlements(ctx, customerID, billing.CheckEntitlementsParams{Prefixes: make([]string, billing.MaxEntitlementPrefixes+1)})
			return err
		},
		"effective tier customer": func() error { _, err := c.GetEffectiveTier(ctx, billing.CustomerID{}, "group"); return err },
		"subscription payment method": func() error {
			_, err := c.SetSubscriptionPaymentMethod(ctx, billing.SubscriptionID{}, billing.SetSubscriptionPaymentMethodParams{PaymentMethodID: method})
			return err
		},
		"payment method for subscription": func() error {
			_, err := c.SetSubscriptionPaymentMethod(ctx, subscription, billing.SetSubscriptionPaymentMethodParams{})
			return err
		},
		"tier preview subscription": func() error {
			_, err := c.PreviewTierChange(ctx, billing.SubscriptionID{}, billing.ChangeTierParams{PriceID: price})
			return err
		},
		"tier preview price": func() error {
			_, err := c.PreviewTierChange(ctx, subscription, billing.ChangeTierParams{})
			return err
		},
		"tier change price": func() error {
			_, err := c.ChangeTier(ctx, subscription, billing.ChangeTierParams{IdempotencyKey: "key"})
			return err
		},
		"delete payment method": func() error {
			_, err := c.DeletePaymentMethod(ctx, billing.CustomerID(uuid.New()), billing.PaymentMethodID{})
			return err
		},
		"empty delegations customer": func() error { _, err := c.SetSpendDelegations(ctx, billing.CustomerID{}, nil); return err },
	}
	// Typed customer ids: the zero id names nobody.
	zero := billing.CustomerID{}
	typedCustomerScoped := map[string]func() error{
		"balance":       func() error { _, err := c.GetBalance(ctx, zero, "USD"); return err },
		"usage":         func() error { _, err := c.GetUsage(ctx, zero, billing.GetUsageParams{Currency: "USD"}); return err },
		"credit grants": func() error { _, err := c.ListCreditGrants(ctx, zero, billing.CreditGrantListParams{}); return err },
		"create credit": func() error { _, err := c.CreateCreditGrant(ctx, zero, billing.CreateCreditGrantParams{}); return err },
		"transactions": func() error {
			_, err := c.ListCreditTransactions(ctx, zero, billing.CreditTransactionListParams{})
			return err
		},
		"delegations": func() error { _, err := c.ListSpendDelegations(ctx, zero); return err },
		"set delegation": func() error {
			_, err := c.SetSpendDelegation(ctx, zero, billing.SpendDelegation{Scope: billing.SpendDelegationInvoker, ScopeKey: "k"})
			return err
		},
		"customer": func() error { _, err := c.GetCustomers(ctx, []billing.CustomerID{zero}); return err },
		"ensure customer": func() error {
			_, err := c.EnsureCustomers(ctx, []billing.EnsureCustomerParams{{ID: zero}})
			return err
		},
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
		"get reprice batch":    func() error { _, err := c.GetRepriceBatch(ctx, billing.RepriceBatchID{}); return err },
		"cancel reprice batch": func() error { _, err := c.CancelRepriceBatch(ctx, billing.RepriceBatchID{}); return err },
		"get reprice":          func() error { _, err := c.GetReprice(ctx, billing.RepriceID{}); return err },
		"cancel reprice":       func() error { _, err := c.CancelReprice(ctx, billing.RepriceID{}); return err },
		"acknowledge host event": func() error {
			_, err := c.AcknowledgeHostEvents(ctx, []billing.HostEventID{{}})
			return err
		},
		"mark notification read": func() error {
			_, err := c.MarkNotificationsRead(ctx, []billing.NotificationID{{}})
			return err
		},
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
