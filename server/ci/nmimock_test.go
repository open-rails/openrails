//go:build e2e && integration

package ci_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/openrailstest/nmimock"
)

// A host's own suite wires OpenRails to the public NMI fake as its package
// comment shows: a customer saves a card and buys with it, and the gateway
// holds the one sale.
func TestHostNMIFakeSavedCardSale(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	gateway := nmimock.New(nmimock.Options{})
	t.Cleanup(gateway.Close)

	cfg := f.config()
	cfg.ProviderWriteMode = openrails.ProviderWritesFull
	cfg.ProviderSandbox = &openrails.ProviderSandboxConfig{NMIGatewayURL: gateway.URL()}
	slug := "nmimock-" + uuid.NewString()[:8]
	cfg.Merchant = openrails.MerchantDeclaration{Slug: slug, DisplayName: slug, PSPs: map[string]openrails.PSPConfig{
		"nmi": openrails.NMIPSP{AccountID: "test", SecurityKey: "test", WebhookSigningSecret: "test", TokenizationKey: "test"}.PSPConfig(),
	}}
	client, err := openrails.New(ctx, cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close(t.Context())) })

	product, err := client.CreateProduct(ctx, billing.CreateProductParams{Key: "post-" + uuid.NewString()[:8], DisplayName: "Paid post", Entitlements: []string{"content:post"}})
	require.NoError(t, err)
	price, err := client.CreatePrice(ctx, billing.CreatePriceParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 4_990_000, Currency: "USD"})
	require.NoError(t, err)
	psps, err := client.ListPSPs(ctx, billing.PSPListParams{})
	require.NoError(t, err)
	require.Len(t, psps.Items, 1)

	as := authtest.NewAuthorizationServer(t, authtest.WithDeps(func(d *authkit.Deps) { d.Postgres = f.pool }))
	customer := authtest.SignIn(t, as.Client, authtest.NewUser(t, as.Client)).AccessToken
	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, client, openrails.Routes{Auth: as.Client.Authenticator(), Prefix: "/billing"}))
	do := func(method, path string, body any, want int) map[string]any {
		t.Helper()
		var payload bytes.Buffer
		if body != nil {
			require.NoError(t, json.NewEncoder(&payload).Encode(body))
		}
		r := httptest.NewRequest(method, "/billing/v1/me"+path, &payload)
		r.Header.Set("Authorization", "Bearer "+customer)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", uuid.NewString())
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		require.Equal(t, want, w.Code, "%s %s: %s", method, path, w.Body.String())
		var out map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
		return out
	}

	card := nmimock.Card{Brand: "visa", Last4: "4242"}
	method := do(http.MethodPost, "/payment-methods", map[string]any{"psp_id": psps.Items[0].ID, "token": gateway.Tokenize(card),
		"billing_details": map[string]any{"name": "Host Customer", "address": map[string]any{"postal_code": "10001", "country": "US"}}}, http.StatusCreated)["id"]
	paid := do(http.MethodPost, "/orders", map[string]any{"lines": []any{map[string]any{"price_id": price.ID}}, "expected_total": "4990000", "payment": map[string]any{"payment_method_id": method}}, http.StatusCreated)
	require.Equal(t, "complete", paid["status"], "%v", paid)

	sale := gateway.LastSale()
	require.NotNil(t, sale)
	require.Equal(t, card.Last4, sale.Last4)
	require.Equal(t, "4.99", sale.Amount)
	require.Len(t, gateway.Ledger(sale.Vault), 1)
	require.EqualValues(t, 499, gateway.Charged(sale.Vault, ""))
	require.Nil(t, gateway.LastDecline())
	require.Empty(t, gateway.Unexpected())
}
