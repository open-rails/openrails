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
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/openrailstest/stripemock"
)

// A host's own suite wires OpenRails to the public Stripe fake as its package
// comment shows: a customer deposits credit of their chosen amount through
// Stripe's hosted Checkout, Stripe's signed checkout.session.completed
// reaches the host's webhook route, and the credit lands once however often
// Stripe sends it.
func TestHostStripeFakeCreditDeposit(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	stripe := stripemock.New(stripemock.Options{})
	t.Cleanup(stripe.Close)

	cfg := f.config()
	cfg.ProviderWriteMode = openrails.ProviderWritesFull
	cfg.ProviderSandbox = &openrails.ProviderSandboxConfig{StripeAPIURL: stripe.URL()}
	slug := "stripemock-" + uuid.NewString()[:8]
	cfg.Merchant = openrails.MerchantDeclaration{Slug: slug, DisplayName: slug, PSPs: map[string]openrails.PSPConfig{
		"stripe": openrails.StripePSP{AccountID: "acct_test", SecretKey: "sk_test_x", WebhookSigningSecret: "whsec_test"}.PSPConfig(),
	}}
	bill, client := f.boot(t, cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool})

	product, err := client.CreateProduct(ctx, billing.CreateProductParams{Key: "credit-" + uuid.NewString()[:8], DisplayName: "API credit", CreditGrant: &catalog.CreditGrantSpec{Currency: "USD", FromPayment: true}})
	require.NoError(t, err)
	price, err := client.CreatePrice(ctx, billing.CreatePriceParams{ProductID: product.ID, Key: "deposit", Currency: "USD", CustomerAmount: &catalog.CustomerAmount{MinAmount: 1_000_000, MaxAmount: 500_000_000}})
	require.NoError(t, err)

	as := authtest.NewAuthorizationServer(t, authtest.WithDeps(func(d *authkit.Deps) { d.Postgres = f.pool }))
	user := authtest.NewUser(t, as.Client)
	customer := authtest.SignIn(t, as.Client, user).AccessToken
	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, bill, openrails.Routes{Auth: as.Client.Authenticator(), Prefix: "/billing"}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	stripe.SendWebhooksTo(srv.URL+"/billing/v1/webhooks/stripe/acct_test", "whsec_test")
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

	paying := do(http.MethodPost, "/orders", map[string]any{"lines": []any{map[string]any{"price_id": price.ID, "amount": "12990000"}}, "expected_total": "12990000",
		"payment": map[string]any{"return_url": "https://e2e.test/done"}}, http.StatusCreated)
	payment := paying["payment"].(map[string]any)
	require.Equal(t, "requires_action", payment["status"], "%v", paying)
	checkout := stripe.LastCheckoutSession()
	require.NotNil(t, checkout)
	require.Equal(t, checkout.URL, payment["next_action"].(map[string]any)["url"], "the customer goes to Stripe's page")
	require.EqualValues(t, 1299, checkout.AmountTotal)

	event, err := stripe.CompleteCheckoutSession(ctx, checkout.ID)
	require.NoError(t, err)
	require.NoError(t, stripe.Redeliver(ctx, event))
	cid := billing.CustomerID(uuid.MustParse(user.ID))
	grants, err := client.ListCreditGrants(ctx, billing.CreditGrantListParams{CustomerID: cid})
	require.NoError(t, err)
	require.Len(t, grants.Items, 1, "one deposit, one lot")
	require.EqualValues(t, 12_990_000, grants.Items[0].Amount, "the chosen amount is the credit")
	balance, err := client.GetBalance(ctx, cid, "USD")
	require.NoError(t, err)
	require.EqualValues(t, 12_990_000, balance.BalanceAmount)
	require.Len(t, stripe.Ledger(""), 1)
	require.Equal(t, "complete", do(http.MethodGet, "/orders/"+paying["id"].(string), nil, http.StatusOK)["status"])
	require.Empty(t, stripe.Unexpected())
}
