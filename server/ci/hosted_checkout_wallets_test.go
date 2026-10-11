//go:build e2e && integration

package ci_test

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/authkit"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/hostedcheckout"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/open-rails/openrails/internal/vaulttest"
	"github.com/open-rails/openrails/server"
)

// Stripe's https://stripe.com/files/apple-pay/apple-developer-merchantid-domain-association.
const stripeApplePayAssociationSHA256 = "2de7b483319c713bf649352f7f158f1fedbda92f546bebfe576ae0a2d2c526c6"

// Declaring a Stripe PSP on a server with a hosted checkout registers the
// checkout host as the account's payment-method domain through a provider
// intent, once; a domain the merchant registered already is validated, not
// registered again. The host serves Stripe's Apple Pay file.
func TestHostedCheckoutRegistersItsHostForWallets(t *testing.T) {
	f := newFixture(t)
	clock := clockwork.NewFakeClockAt(time.Now().UTC().Truncate(time.Second))
	stripe := stripemock.New(stripemock.Options{Clock: clock.Now})
	t.Cleanup(stripe.Close)
	vault := vaulttest.New(t)
	srv := f.newServer(t, func(cfg *server.Config, deps *server.Deps) {
		cfg.Engine.ProviderWriteMode = openrails.ProviderWritesFull
		cfg.Engine.Vault = vault.Config()
		cfg.Engine.ProviderSandbox = &openrails.ProviderSandboxConfig{StripeAPIURL: stripe.URL()}
		cfg.Engine.PublicBillingBaseURL = "https://" + apiHost
		cfg.Engine.RateLimitsDisabled = true
		cfg.Auth.Resource = authkit.ResourceConfig{ID: resourceID, PublicURL: rsOrigin}
		cfg.HostedCheckoutURL = "https://" + checkoutHost
		deps.Engine.Clock = clock
		deps.CheckoutAssets = checkoutPage
	})
	handler, err := standaloneHandler(srv)
	require.NoError(t, err)

	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "https://"+checkoutHost+hostedcheckout.ApplePayDomainAssociationPath, nil))
	require.Equal(t, http.StatusOK, res.Code)
	sum := sha256.Sum256(res.Body.Bytes())
	require.Equal(t, stripeApplePayAssociationSHA256, hex.EncodeToString(sum[:]))
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "https://"+apiHost+hostedcheckout.ApplePayDomainAssociationPath, nil))
	require.Equal(t, http.StatusNotFound, res.Code, "only the checkout host serves it")

	entry, err := srv.ProvisionMerchant(t.Context(), billing.ProvisionMerchantParams{Slug: uniqueName("wallets"), DisplayName: "Wallet Shop"})
	require.NoError(t, err)
	shop, ctx := handle(t, srv, entry.MerchantID), merchant.WithID(t.Context(), entry.MerchantID)
	stripe.SeedPaymentMethodDomain("acct_known", checkoutHost, false)
	declare := func(key, account string) billing.PSPID {
		psp, err := shop.CreatePSP(t.Context(), billing.CreatePSPParams{Key: key, Rail: billing.RailStripe, AccountID: account,
			Settings:    map[string]any{"publishable_key": "pk_test_" + key},
			Credentials: map[string]string{"secret_key": "sk_test_" + account, "webhook_signing_secret": "whsec_" + key}})
		require.NoError(t, err)
		return psp.ID
	}
	fresh, known := declare("stripe", "acct_fresh"), declare("known", "acct_known")

	rt := engine.Graph(srv.Engine()).Runtime
	store := intents.NewStore(rt.DB)
	run := func(psp billing.PSPID) gen.BillingProviderIntent {
		t.Helper()
		var row gen.BillingProviderIntent
		require.Eventually(t, func() bool {
			row, err = store.GetByIdempotencyKey(ctx, intents.StripePaymentMethodDomainKey(psp.UUID(), checkoutHost))
			return err == nil
		}, 10*time.Second, 20*time.Millisecond, "declaring the PSP queues its registration")
		require.Equal(t, intents.TypeStripePaymentMethodDomain, row.IntentType)
		row, err = rt.IntentRunner().ExecuteByID(ctx, row.ID)
		require.NoError(t, err)
		require.Equal(t, intents.StatusSucceeded, row.Status)
		return row
	}

	registered := run(fresh)
	domains := stripe.PaymentMethodDomains("acct_fresh")
	require.Len(t, domains, 1)
	require.Equal(t, checkoutHost, domains[0]["domain_name"])
	require.Equal(t, true, domains[0]["enabled"])
	creates := stripe.Submitted("/v1/payment_method_domains")
	require.Len(t, creates, 1)
	require.Equal(t, registered.IdempotencyKey, creates[0].IdempotencyKey)
	require.Equal(t, checkoutHost, creates[0].Form.Get("domain_name"))

	run(known)
	domains = stripe.PaymentMethodDomains("acct_known")
	require.Len(t, domains, 1, "a domain the account holds is not registered again")
	require.Equal(t, map[string]any{"status": "active"}, map[string]any(domains[0]["apple_pay"].(stripemock.Object)))
	require.Len(t, stripe.Submitted("/v1/payment_method_domains"), 1)
	require.Len(t, stripe.Submitted("/v1/payment_method_domains/"+domains[0]["id"].(string)+"/validate"), 1)

	// A merchant's next checkout finds both registered and queues nothing.
	require.NoError(t, rt.RegisterCheckoutDomain(t.Context(), entry.MerchantID))
	for _, psp := range []billing.PSPID{fresh, known} {
		row, err := store.GetByIdempotencyKey(ctx, intents.StripePaymentMethodDomainKey(psp.UUID(), checkoutHost))
		require.NoError(t, err)
		require.Equal(t, intents.StatusSucceeded, row.Status)
	}
	require.Len(t, stripe.Submitted("/v1/payment_method_domains"), 1)

	// Sites registered for embedded checkouts are registered too: a wallet
	// shows in a frame only when the framing page's domain is. Loopback
	// sites are not.
	_, err = shop.UpdateMerchantConfiguration(t.Context(), billing.UpdateMerchantConfigurationParams{
		Settings: &billing.MerchantSettings{Checkout: &billing.MerchantCheckoutSettings{EmbedOrigins: []string{"https://shop.e2e.test", "http://localhost:3000"}}}})
	require.NoError(t, err)
	for _, psp := range []billing.PSPID{fresh, known} {
		require.Eventually(t, func() bool {
			_, err := store.GetByIdempotencyKey(ctx, intents.StripePaymentMethodDomainKey(psp.UUID(), "shop.e2e.test"))
			return err == nil
		}, 10*time.Second, 20*time.Millisecond, "registering an embed origin queues its registration")
		_, err = store.GetByIdempotencyKey(ctx, intents.StripePaymentMethodDomainKey(psp.UUID(), "localhost"))
		require.Error(t, err)
	}
	row, err := store.GetByIdempotencyKey(ctx, intents.StripePaymentMethodDomainKey(fresh.UUID(), "shop.e2e.test"))
	require.NoError(t, err)
	row, err = rt.IntentRunner().ExecuteByID(ctx, row.ID)
	require.NoError(t, err)
	require.Equal(t, intents.StatusSucceeded, row.Status)
	names := []any{}
	for _, d := range stripe.PaymentMethodDomains("acct_fresh") {
		names = append(names, d["domain_name"])
	}
	require.ElementsMatch(t, []any{checkoutHost, "shop.e2e.test"}, names)
}
