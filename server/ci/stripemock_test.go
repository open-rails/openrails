//go:build e2e && integration

package ci_test

import (
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
	client, err := openrails.New(ctx, cfg, openrails.Deps{Postgres: f.pool})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close(t.Context())) })

	product, err := client.CreateProduct(ctx, billing.CreateProductParams{Key: "credit-" + uuid.NewString()[:8], DisplayName: "API credit", CreditGrant: &catalog.CreditGrantSpec{Currency: "USD", FromPayment: true}})
	require.NoError(t, err)
	price, err := client.CreatePrice(ctx, billing.CreatePriceParams{ProductID: product.ID, Key: "deposit", Currency: "USD", CustomerAmount: &catalog.CustomerAmount{MinAmount: 1_000_000, MaxAmount: 500_000_000}})
	require.NoError(t, err)

	as := authtest.NewAuthorizationServer(t, authtest.WithDeps(func(d *authkit.Deps) { d.Postgres = f.pool }))
	user := authtest.NewUser(t, as.Client)
	customer := authtest.SignIn(t, as.Client, user).AccessToken
	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, client, openrails.Routes{Auth: as.Client, Prefix: "/billing"}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	stripe.SendWebhooksTo(srv.URL+"/billing/v1/webhooks/stripe/acct_test", "whsec_test")
	do := func(method, path string, body any, want int) map[string]any {
		t.Helper()
		w := call(t, mux, customer, method, "/billing/v1/me"+path, "", body)
		require.Equal(t, want, w.Code, "%s %s: %s", method, path, w.Body.String())
		var out map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
		return out
	}

	session := do(http.MethodPost, "/checkout-sessions", map[string]any{"price_id": price.ID, "amount": "12990000", "success_url": "https://e2e.test/done"}, http.StatusCreated)["id"].(string)
	var option any
	for _, raw := range do(http.MethodGet, "/checkout-sessions/"+session, nil, http.StatusOK)["options"].([]any) {
		if o := raw.(map[string]any); o["rail"] == "stripe" {
			option = o["id"]
		}
	}
	require.NotNil(t, option, "the session offers the Stripe PSP")
	paying := do(http.MethodPost, "/checkout-sessions/"+session+"/pay", map[string]any{"option_id": option}, http.StatusOK)
	require.Equal(t, "requires_action", paying["status"], "%v", paying)
	checkout := stripe.LastCheckoutSession()
	require.NotNil(t, checkout)
	require.Equal(t, checkout.URL, paying["next_action"].(map[string]any)["url"], "the customer goes to Stripe's page")
	require.EqualValues(t, 1299, checkout.AmountTotal)

	event, err := stripe.CompleteCheckoutSession(ctx, checkout.ID)
	require.NoError(t, err)
	require.NoError(t, stripe.Redeliver(ctx, event))
	cid := billing.CustomerID(uuid.MustParse(user.ID))
	grants, err := client.ListCreditGrants(ctx, cid, billing.CreditGrantListParams{})
	require.NoError(t, err)
	require.Len(t, grants.Items, 1, "one deposit, one lot")
	require.EqualValues(t, 12_990_000, grants.Items[0].Amount, "the chosen amount is the credit")
	balance, err := client.GetBalance(ctx, cid, "USD")
	require.NoError(t, err)
	require.EqualValues(t, 12_990_000, balance.BalanceAmount)
	require.Len(t, stripe.Ledger(""), 1)
	require.Equal(t, "succeeded", do(http.MethodGet, "/checkout-sessions/"+session, nil, http.StatusOK)["status"])
	require.Empty(t, stripe.Unexpected())
}
