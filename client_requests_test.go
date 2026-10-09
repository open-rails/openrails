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
		"/v1/app/admissions":         `{"items":[{"status":200,"admission":{"allowed":true,"state":"open"},"error":null}]}`,
		"/v1/admin/customers":        `{"data":[{"id":"` + customer + `","settings":{"customer_id":"` + customer + `","credit_limits":[],"trust_levels":[{"currency":"USD","trust_level":"gold"}],"billing_policy":null,"invoice_profile":null}}],"next_cursor":null}`,
		"/v1/admin/credit-grants":    `{"data":[{"amount":"1"}],"next_cursor":null}`,
		"/v1/admin/product-access":   `{"data":[],"next_cursor":"next"}`,
		"/v1/admin/entitlements":     `{"data":[{"customer_id":"` + customer + `","entitlement":"pro"}],"next_cursor":null}`,
		"/v1/app/entitlements/check": `{"entitlements":{"pro":true,"team":false}}`,
	})
	who := billing.CheckoutCustomerIdentity{ID: billing.CustomerID(uuid.MustParse(customer))}
	expires := time.Now().Add(time.Hour)
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
		}, http.MethodPost, "/v1/admin/checkout-sessions", "", func(t *testing.T, b map[string]any) {
			require.Equal(t, "pro-monthly", b["price_key"])
			require.Equal(t, who.ID.String(), b["customer"].(map[string]any)["id"])
		}},
		{"checkout options for a price", func() error {
			_, err := client.ListCheckoutOptions(t.Context(), billing.CheckoutOptionListParams{ProductKey: "pro", PriceKey: "pro-monthly"})
			return err
		}, http.MethodGet, "/v1/admin/checkout-options", "price_key=pro-monthly&product_key=pro", nil},
		{"settings carry named policies and tier bindings", func() error {
			revision := "r1"
			_, err := client.UpdateMerchantConfiguration(t.Context(), billing.UpdateMerchantConfigurationParams{IdempotencyKey: "a1", ExpectedRevision: &revision, Settings: &billing.MerchantSettings{
				BillingPolicies:       []billing.BillingPolicy{{Name: "api_line", Kind: "outstanding_cap", OutstandingCapAmount: 200_000_000}},
				BillingPolicyBindings: []billing.BillingPolicyBinding{{PolicyName: "api_line", Tier: "gold"}},
			}})
			return err
		}, http.MethodPatch, "/v1/admin/configuration", "", func(t *testing.T, b map[string]any) {
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
		}, http.MethodPost, "/v1/app/admissions", "", func(t *testing.T, b map[string]any) {
			item := b["items"].([]any)[0].(map[string]any)
			require.Equal(t, "gold", item["trust_level"])
			require.Equal(t, "42", item["accrual_rate_delta_per_hour"])
		}},
		{"customer read names each customer", func() error {
			page, err := client.ListCustomers(t.Context(), billing.CustomerListParams{IDs: []billing.CustomerID{typedCustomer, typedCustomer}})
			if err == nil && page.Items[0].Settings.TrustLevels[0].TrustLevel != "gold" {
				err = errors.New("settings not decoded")
			}
			return err
		}, http.MethodGet, "/v1/admin/customers", "ids=" + customer + "%2C" + customer, nil},
		{"overdue invoices", func() error {
			_, err := client.ListInvoices(t.Context(), billing.InvoiceListParams{Overdue: true})
			return err
		}, http.MethodGet, "/v1/admin/invoices", "overdue=true", nil},
		{"subscriptions in dunning", func() error {
			_, err := client.ListSubscriptions(t.Context(), billing.SubscriptionListParams{Dunning: true})
			return err
		}, http.MethodGet, "/v1/admin/subscriptions", "dunning=true", nil},
		{"settings write sends only the named fields", func() error {
			_, err := client.UpdateCustomer(t.Context(), typedCustomer, billing.UpdateCustomerParams{BillingPolicy: catalog.Null[string]()})
			return err
		}, http.MethodPatch, "/v1/admin/customers/" + customer, "", func(t *testing.T, b map[string]any) {
			require.Equal(t, map[string]any{"billing_policy": nil}, b)
		}},
		{"credit grant source ids are opaque query values", func() error {
			page, err := client.ListCreditGrants(t.Context(), billing.CreditGrantListParams{CustomerID: typedCustomer, SourceID: "../source/receipt?part=1&currency=JPY"})
			if err == nil && page.Items[0].Amount != 1 {
				err = errors.New("grant amount not decoded")
			}
			return err
		}, http.MethodGet, "/v1/admin/credit-grants", "customer_id=" + customer + "&source_id=..%2Fsource%2Freceipt%3Fpart%3D1%26currency%3DJPY", nil},
		{"list ids are one comma list beside nothing else", func() error {
			_, err := client.ListPayments(t.Context(), billing.PaymentListParams{IDs: []billing.PaymentID{billing.PaymentID(uuid.MustParse(customer)), billing.PaymentID(uuid.MustParse(customer))}})
			return err
		}, http.MethodGet, "/v1/admin/payments", "ids=pay_" + customer + "%2Cpay_" + customer, nil},
		{"credit grants batch across customers", func() error {
			_, err := client.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: typedCustomer, Currency: " usd ", Amount: 1, Source: "s", SourceID: "1"}})
			return err
		}, http.MethodPost, "/v1/admin/credit-grants", "", func(t *testing.T, b map[string]any) {
			item := b["items"].([]any)[0].(map[string]any)
			require.Equal(t, customer, item["customer_id"])
			require.Equal(t, "USD", item["currency"])
		}},
		{"admission releases name request ids", func() error {
			_, err := client.ReleaseAdmissions(t.Context(), []string{"../a", "b"})
			return err
		}, http.MethodPost, "/v1/app/admissions/release", "", func(t *testing.T, b map[string]any) {
			require.Equal(t, []any{"../a", "b"}, b["request_ids"])
		}},
		{"entitlements of named customers", func() error {
			page, err := client.ListEntitlements(t.Context(), billing.EntitlementListParams{CustomerIDs: []billing.CustomerID{customerID, customerID}, Entitlements: []string{"pro", "a,b"}})
			if err == nil && (len(page.Items) != 1 || page.Items[0].CustomerID != customerID || page.Items[0].Entitlement != "pro") {
				err = errors.New("entitlements not decoded")
			}
			return err
		}, http.MethodGet, "/v1/admin/entitlements", "customer_id=" + customer + "%2C" + customer + "&entitlement=pro&entitlement=a%2Cb", nil},
		{"holders of one entitlement", func() error {
			_, err := client.ListEntitlements(t.Context(), billing.EntitlementListParams{Entitlements: []string{"pro"}, PageRequest: billing.PageRequest{Limit: 7}})
			return err
		}, http.MethodGet, "/v1/admin/entitlements", "entitlement=pro&limit=7", nil},
		{"entitlement check", func() error {
			got, err := client.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: customerID, Entitlements: []string{"pro", "team"}})
			if err == nil && (!got.Entitlements["pro"] || got.Entitlements["team"] || len(got.Entitlements) != 2 || got.Held == nil) {
				err = errors.New("check not decoded")
			}
			return err
		}, http.MethodPost, "/v1/app/entitlements/check", "", func(t *testing.T, b map[string]any) {
			require.Equal(t, map[string]any{"customer_id": customer, "entitlements": []any{"pro", "team"}}, b, "zero At, prefixes and limit are omitted")
		}},
		{"entitlement check prefixes only", func() error {
			_, err := client.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: customerID, Prefixes: []string{"content:t:"}, PrefixLimit: 5})
			return err
		}, http.MethodPost, "/v1/app/entitlements/check", "", func(t *testing.T, b map[string]any) {
			require.Equal(t, map[string]any{"customer_id": customer, "entitlements": []any{}, "prefixes": []any{"content:t:"}, "prefix_limit": float64(5)}, b, "no keys send a list, not null")
		}},
		{"access list pages", func() error {
			id, _ := billing.ParseProductID(product)
			page, err := client.ListProductAccess(t.Context(), billing.ProductAccessListParams{PageRequest: billing.PageRequest{Limit: 7, Cursor: "cursor"}, CustomerIDs: []billing.CustomerID{customerID}, ProductIDs: []billing.ProductID{id}, LiveOnly: true})
			if err == nil && page.Next != "next" {
				err = errors.New("page not decoded")
			}
			return err
		}, http.MethodGet, "/v1/admin/product-access", "cursor=cursor&customer_id=" + customer + "&limit=7&live=true&product_id=" + product, nil},
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
		"meter":     func(id string) error { _, err := c.GetMeter(ctx, id); return err },
		"set meter": func(id string) error { _, err := c.SetMeter(ctx, id, billing.SetMeterParams{}); return err },
		"rate override meter": func(id string) error {
			_, err := c.SetRateOverride(ctx, billing.CustomerID(uuid.New()), id, billing.SetRateOverrideParams{})
			return err
		},
		"provider operation": func(id string) error { _, err := c.GetProviderOperation(ctx, id); return err },
	}
	// Opaque host strings (request ids, deposit keys, migration prices): only blankness is refused.
	blankOnly := map[string]func(id string) error{
		"preview migration product key": func(id string) error {
			_, err := c.PreviewPriceMigration(ctx, billing.CreatePriceMigrationParams{ProductKey: id, PriceKey: "monthly"})
			return err
		},
		"preview migration price key": func(id string) error {
			_, err := c.PreviewPriceMigration(ctx, billing.CreatePriceMigrationParams{ProductKey: "pro", PriceKey: id})
			return err
		},
		"capture": func(id string) error {
			_, err := c.CaptureAdmission(ctx, id, billing.CaptureAdmissionParams{Amount: 1})
			return err
		},
		"entitlement key": func(id string) error {
			_, err := c.ListEntitlements(ctx, billing.EntitlementListParams{CustomerIDs: []billing.CustomerID{customerID}, Entitlements: []string{"pro", id}, At: now})
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
		"retry invoice": func() error {
			_, err := c.RetryInvoiceCollection(ctx, billing.InvoiceID{}, billing.RetryInvoiceCollectionParams{IdempotencyKey: "k"})
			return err
		},
		"payment methods customer": func() error {
			_, err := c.ListPaymentMethods(ctx, billing.CustomerID{}, billing.PaymentMethodListParams{})
			return err
		},
		"empty customer ids": func() error {
			_, err := c.ListCustomers(ctx, billing.CustomerListParams{IDs: []billing.CustomerID{}})
			return err
		},
		"settings update customer": func() error {
			_, err := c.UpdateCustomer(ctx, billing.CustomerID{}, billing.UpdateCustomerParams{})
			return err
		},
		"order":        func() error { _, err := c.GetOrder(ctx, billing.OrderID{}); return err },
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
		"checkout options": func() error {
			_, err := c.ListCheckoutOptions(ctx, billing.CheckoutOptionListParams{PriceID: billing.PriceID(uuid.New()), PriceKey: "k"})
			return err
		},
		"checkout options without a price": func() error {
			_, err := c.ListCheckoutOptions(ctx, billing.CheckoutOptionListParams{})
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
		"access list customer": func() error {
			_, err := c.ListProductAccess(ctx, billing.ProductAccessListParams{CustomerIDs: []billing.CustomerID{{}}})
			return err
		},
		"access list empty customers": func() error {
			_, err := c.ListProductAccess(ctx, billing.ProductAccessListParams{CustomerIDs: []billing.CustomerID{}})
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
		"revoke access id": func() error {
			return c.RevokeProductAccess(ctx, billing.ProductAccessID{}, billing.RevokeProductAccessParams{Reason: "r"})
		},
		"list entitlements customer": func() error {
			_, err := c.ListEntitlements(ctx, billing.EntitlementListParams{CustomerIDs: []billing.CustomerID{{}}})
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
		"list ids empty": func() error {
			_, err := c.ListCustomers(ctx, billing.CustomerListParams{IDs: []billing.CustomerID{}})
			return err
		},
		"list ids over bound": func() error {
			ids := make([]billing.PaymentID, billing.MaxBatchItems+1)
			for i := range ids {
				ids[i] = billing.PaymentID(uuid.New())
			}
			_, err := c.ListPayments(ctx, billing.PaymentListParams{IDs: ids})
			return err
		},
		"list ids zero": func() error {
			_, err := c.ListInvoices(ctx, billing.InvoiceListParams{IDs: []billing.InvoiceID{{}}})
			return err
		},
		"release empty": func() error {
			_, err := c.ReleaseAdmissions(ctx, nil)
			return err
		},
		"extend empty": func() error {
			_, err := c.ExtendAdmissions(ctx, nil)
			return err
		},
		"extend over bound": func() error {
			_, err := c.ExtendAdmissions(ctx, make([]billing.ExtendAdmissionParams, billing.MaxAdmissionBatchItems+1))
			return err
		},
		"credit grants empty": func() error {
			_, err := c.CreateCreditGrants(ctx, nil)
			return err
		},
		"host events empty": func() error {
			_, err := c.AcknowledgeHostEvents(ctx, nil)
			return err
		},
		"entitlements over bound": func() error {
			_, err := c.ListEntitlements(ctx, billing.EntitlementListParams{CustomerIDs: []billing.CustomerID{customerID}, Entitlements: make([]string, billing.MaxBatchItems+1)})
			return err
		},
		"entitlements customers over bound": func() error {
			_, err := c.ListEntitlements(ctx, billing.EntitlementListParams{CustomerIDs: make([]billing.CustomerID, billing.MaxBatchItems+1)})
			return err
		},
		"entitlement holders of several keys": func() error {
			_, err := c.ListEntitlements(ctx, billing.EntitlementListParams{Entitlements: []string{"a", "b"}})
			return err
		},
		"entitlement holders under a prefix": func() error {
			_, err := c.ListEntitlements(ctx, billing.EntitlementListParams{Entitlements: []string{"a"}, Prefix: "a"})
			return err
		},
		"subscription payment method": func() error {
			_, err := c.SetSubscriptionPaymentMethod(ctx, billing.SubscriptionID{}, billing.SetSubscriptionPaymentMethodParams{PaymentMethodID: &method})
			return err
		},
		"payment method for subscription": func() error {
			_, err := c.SetSubscriptionPaymentMethod(ctx, subscription, billing.SetSubscriptionPaymentMethodParams{PaymentMethodID: &billing.PaymentMethodID{}})
			return err
		},
		"change preview subscription": func() error {
			_, err := c.PreviewSubscriptionChange(ctx, billing.SubscriptionID{}, billing.ChangeSubscriptionParams{PriceID: &price})
			return err
		},
		"change preview of nothing": func() error {
			_, err := c.PreviewSubscriptionChange(ctx, subscription, billing.ChangeSubscriptionParams{})
			return err
		},
		"change of nothing": func() error {
			_, err := c.ChangeSubscription(ctx, subscription, billing.ChangeSubscriptionParams{IdempotencyKey: "key"})
			return err
		},
	}
	// Typed customer ids: the zero id names nobody.
	zero := billing.CustomerID{}
	typedCustomerScoped := map[string]func() error{
		"balance": func() error { _, err := c.GetBalance(ctx, zero, "USD"); return err },
		"credit grants": func() error {
			_, err := c.ListCreditGrants(ctx, billing.CreditGrantListParams{CustomerID: zero})
			return err
		},
		"create credit": func() error {
			_, err := c.CreateCreditGrants(ctx, []billing.CreateCreditGrantParams{{CustomerID: zero}})
			return err
		},
		"transactions": func() error {
			_, err := c.ListBalanceTransactions(ctx, zero, billing.BalanceTransactionListParams{})
			return err
		},
		"customer": func() error {
			_, err := c.ListCustomers(ctx, billing.CustomerListParams{IDs: []billing.CustomerID{zero}})
			return err
		},
		"provisioning token": func() error { return c.DeleteProvisioningToken(ctx, billing.ProvisioningTokenID{}) },
		"get customer":       func() error { _, err := c.GetCustomer(ctx, zero); return err },
		"credit grant": func() error {
			_, err := c.GetCreditGrant(ctx, billing.CreditGrantID{})
			return err
		},
		"revoke credit": func() error {
			_, err := c.RevokeCreditGrant(ctx, billing.CreditGrantID{}, billing.RevokeCreditGrantParams{})
			return err
		},
	}
	uuidCalls := map[string]func() error{
		"get price migration":    func() error { _, err := c.GetPriceMigration(ctx, billing.PriceMigrationID{}); return err },
		"cancel price migration": func() error { _, err := c.CancelPriceMigration(ctx, billing.PriceMigrationID{}); return err },
		"acknowledge host event": func() error {
			_, err := c.AcknowledgeHostEvents(ctx, []billing.HostEventID{{}})
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
