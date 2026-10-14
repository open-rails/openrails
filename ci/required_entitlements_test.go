//go:build e2e && integration

package ci_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/openrailstest/nmimock"
)

// A members-only post sells only to customers who hold the channel's
// membership when they order and when they pay. A member buys it through its
// pay URL; a non-member is refused on the customer's route, in a preview and
// on the merchant's own order; a member whose membership is revoked before
// paying is refused on the payment page, and asking again for the open order is
// refused too. The catalog document declares the requirement; an edit then
// takes the field, and the document no longer changes it.
func TestRequiredEntitlementsGateOrders(t *testing.T) {
	f := newFixture(t)
	gateway := nmimock.New(nmimock.Options{})
	t.Cleanup(gateway.Close)
	cfg := f.config()
	cfg.ProviderWriteMode = openrails.ProviderWritesFull
	cfg.PublicBillingBaseURL = "https://api.e2e.test"
	cfg.ProviderSandbox = &openrails.ProviderSandboxConfig{NMIGatewayURL: gateway.URL()}
	cfg.RateLimitsDisabled = true
	cfg.Merchants = map[string]openrails.MerchantDeclaration{"members-only": {
		DisplayName:      "Members only",
		MerchantSettings: billing.MerchantSettings{PaymentPage: &billing.MerchantPaymentPageSettings{EmbedOrigins: []string{"https://shop.e2e.test"}}},
		PSPs:             map[string]openrails.PSPConfig{"nmi": nmiPSP()},
	}}
	bill, shop := f.boot(t, cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool})
	_, paymentHost, err := bill.PaymentPage("https://pay.e2e.test", fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>Payment</title>")}})
	require.NoError(t, err)
	auth := &authtest.Fake{}
	app := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(app, bill, openrails.Routes{Auth: auth}))
	ctx := t.Context()

	const member, post = "channel:c1:member", "post:p1"
	document := func(description string) *catalog.Application {
		doc, err := catalog.ParseApplicationYAML([]byte(`
schema_version: 1
products:
  membership:
    display_name: Channel membership
    entitlements: ["` + member + `"]
  members-post:
    display_name: Members-only post
    description: ` + description + `
    entitlements: ["` + post + `"]
    required_entitlements: ["` + member + `"]
    prices:
      buy:
        amount: 5 USD
        access_duration: null
`))
		require.NoError(t, err)
		return doc
	}
	receipt, err := shop.ApplyCatalog(ctx, document("For members"), billing.ApplyCatalogParams{})
	require.NoError(t, err)
	require.Empty(t, receipt.Conflicts)
	products, err := shop.ListProducts(ctx, billing.ProductListParams{Keys: []string{"membership", "members-post"}, PageRequest: billing.PageRequest{Limit: 2}})
	require.NoError(t, err)
	byKey := map[string]billing.Product{}
	for _, p := range products.Items {
		byKey[p.Key] = p
	}
	membership, members := byKey["membership"], byKey["members-post"]
	require.Equal(t, []string{member}, members.RequiredEntitlements)
	require.Empty(t, membership.RequiredEntitlements)
	require.Len(t, members.Prices, 1)
	price := members.Prices[0].ID

	call := func(h http.Handler, method, target, bearer string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var payload bytes.Buffer
		if body != nil {
			require.NoError(t, json.NewEncoder(&payload).Encode(body))
		}
		req := httptest.NewRequest(method, target, &payload)
		req.RemoteAddr = "192.0.2.30:1234"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", uuid.NewString())
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		return res
	}
	lines := []map[string]any{{"price_id": price}}
	order := func(token string) *httptest.ResponseRecorder {
		t.Helper()
		return call(app, http.MethodPost, "/v1/me/orders", token, map[string]any{"lines": lines, "pay_url": map[string]any{"embedded": true}})
	}
	secretOf := func(o billing.Order) string {
		t.Helper()
		require.NotNil(t, o.PayURLExpiresAt)
		require.NotNil(t, o.PayURL)
		_, secret, ok := strings.Cut(*o.PayURL, "#")
		require.True(t, ok)
		return secret
	}
	pay := func(o billing.Order, secret string) *httptest.ResponseRecorder {
		t.Helper()
		return call(paymentHost, http.MethodPost, "https://pay.e2e.test/v1/me/orders/"+o.ID.String()+"/pay", secret, map[string]any{
			"payment": map[string]any{
				"token":           gateway.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242"}),
				"billing_details": map[string]any{"name": "Card Holder", "address": map[string]string{"country": "US", "postal_code": "10001"}},
			},
			"expected_total": "5000000",
		})
	}
	refused := func(res *httptest.ResponseRecorder) {
		t.Helper()
		require.Equal(t, http.StatusForbidden, res.Code, res.Body.String())
		var envelope struct {
			Error billing.ErrorDetails `json:"error"`
		}
		require.NoError(t, json.Unmarshal(res.Body.Bytes(), &envelope))
		require.Equal(t, billing.CodeEntitlementRequired, envelope.Error.Code)
		require.NotNil(t, envelope.Error.Param)
		require.Equal(t, "lines[0]", *envelope.Error.Param)
		require.Equal(t, []any{member}, envelope.Error.Metadata["missing"])
	}
	grant := func(customer string) billing.ProductAccessID {
		t.Helper()
		grants, err := shop.CreateProductAccess(ctx, billing.CreateProductAccessBatchParams{Items: []billing.CreateProductAccessParams{{CustomerID: cid(customer), ProductID: membership.ID}}})
		require.NoError(t, err)
		require.Len(t, grants, 1)
		return grants[0].ID
	}
	holds := func(customer, key string) bool {
		t.Helper()
		held, err := heldKeys(ctx, shop, cid(customer), time.Time{}, key)
		require.NoError(t, err)
		return held[key]
	}

	// A member buys it.
	alice := uuid.NewString()
	grant(alice)
	res := order(auth.Person(alice))
	require.Equal(t, http.StatusCreated, res.Code, res.Body.String())
	bought := decode[billing.Order](t, res)
	res = pay(bought, secretOf(bought))
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	require.Equal(t, billing.OrderComplete, decode[billing.Order](t, res).Status)
	require.True(t, holds(alice, post))

	// A non-member is refused, and so is the merchant's order for them; the
	// preview says why. No order is made.
	bob := uuid.NewString()
	bobToken := auth.Person(bob)
	preview := call(app, http.MethodPost, "/v1/me/orders/preview", bobToken, map[string]any{"lines": lines})
	require.Equal(t, http.StatusOK, preview.Code, preview.Body.String())
	refusal := decode[billing.OrderPreview](t, preview).Lines[0].Refusal
	require.NotNil(t, refusal)
	require.Equal(t, billing.CodeEntitlementRequired, refusal.Code)
	require.Equal(t, []string{member}, refusal.Missing)
	refused(order(bobToken))
	_, err = shop.CreateOrder(ctx, billing.CreateOrderParams{CustomerID: cid(bob), Lines: []billing.OrderLineParams{{PriceID: price}}, IdempotencyKey: uuid.NewString()})
	requireCode(t, err, billing.CodeEntitlementRequired)
	require.Empty(t, decode[billing.ListPage[billing.Order]](t, call(app, http.MethodGet, "/v1/me/orders", bobToken, nil)).Items)
	require.False(t, holds(bob, post))

	// A member whose membership is revoked before paying is refused at the
	// purchase, and asking again for the open order is refused too.
	carol := uuid.NewString()
	carolToken := auth.Person(carol)
	access := grant(carol)
	res = order(carolToken)
	require.Equal(t, http.StatusCreated, res.Code, res.Body.String())
	open := decode[billing.Order](t, res)
	require.NoError(t, shop.RevokeProductAccess(ctx, access, billing.RevokeProductAccessParams{Reason: "left the channel"}))
	refused(pay(open, secretOf(open)))
	refused(order(carolToken))
	require.Equal(t, billing.OrderOpen, decode[billing.Order](t, call(app, http.MethodGet, "/v1/me/orders/"+open.ID.String(), carolToken, nil)).Status)
	require.False(t, holds(carol, post))

	// The merchant's payment page for a member who leaves takes no payment.
	dave := uuid.NewString()
	access = grant(dave)
	hosted, err := shop.CreateOrder(ctx, billing.CreateOrderParams{CustomerID: cid(dave), Lines: []billing.OrderLineParams{{PriceID: price}}, IdempotencyKey: uuid.NewString(),
		PayURL: &billing.OrderPayURLParams{SuccessURL: "https://shop.e2e.test/thanks"}})
	require.NoError(t, err)
	require.NoError(t, shop.RevokeProductAccess(ctx, access, billing.RevokeProductAccessParams{Reason: "left the channel"}))
	refused(pay(*hosted, secretOf(*hosted)))
	require.False(t, holds(dave, post))

	// An edit takes the field: a document naming another value is skipped
	// with the conflict, and the edit's value stands.
	vip := "channel:c1:vip"
	edited, err := shop.UpdateProduct(ctx, members.ID, billing.UpdateProductParams{RequiredEntitlements: catalog.Value([]string{vip, member})})
	require.NoError(t, err)
	require.Equal(t, []string{member, vip}, edited.RequiredEntitlements)
	receipt, err = shop.ApplyCatalog(ctx, document("For members only"), billing.ApplyCatalogParams{})
	require.NoError(t, err)
	require.Len(t, receipt.Conflicts, 1)
	require.Equal(t, "members-post", receipt.Conflicts[0].Key)
	require.Len(t, receipt.Conflicts[0].Fields, 1)
	require.Equal(t, "required_entitlements", receipt.Conflicts[0].Fields[0].Field)
	read, err := shop.GetProduct(ctx, members.ID)
	require.NoError(t, err)
	require.Equal(t, []string{member, vip}, read.RequiredEntitlements)
	require.Equal(t, "For members", read.Description)
}
