//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/internal/shared/httpx"
	"github.com/open-rails/openrails/internal/vaulttest"
	"github.com/stretchr/testify/require"
)

// Configuration really lives in a unique KV-v2 mount. Only the external Stripe
// wire is substituted; its journal remains independent from OpenRails' book.
type pspWire struct {
	provider    *providerLedger
	mu          sync.Mutex
	keys        []string
	redirect    string
	liveBalance bool
}

func (p *pspWire) serve(out http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	p.mu.Lock()
	p.keys = append(p.keys, key)
	redirect := p.redirect
	live := p.liveBalance
	p.mu.Unlock()
	if redirect != "" {
		http.Redirect(out, r, redirect, http.StatusTemporaryRedirect)
		return
	}
	if key != "sk_test_critical" && key != "sk_test_rotated" {
		http.Error(out, "fixture rejects this credential", http.StatusUnauthorized)
		return
	}
	if live && r.URL.Path == "/v1/balance" {
		out.Header().Set("Content-Type", "application/json")
		_, _ = out.Write([]byte(`{"object":"balance","livemode":true,"available":[],"pending":[]}`))
		return
	}
	copy := r.Clone(r.Context())
	copy.Header.Set("Authorization", "Bearer sk_test_critical")
	p.provider.serve(out, copy)
}

func newPSPWorld(t *testing.T, configure ...func(*world)) (*authorityWorld, *pspWire) {
	t.Helper()
	addr, token := os.Getenv("OPENRAILS_CRITICAL_VAULT_ADDR"), os.Getenv("OPENRAILS_CRITICAL_VAULT_TOKEN")
	require.NotEmpty(t, addr, "critical configuration cases require real Vault")
	require.NotEmpty(t, token)
	t.Setenv(vaulttest.AddrEnv, addr)
	t.Setenv(vaulttest.TokenEnv, token)
	vault := vaulttest.New(t)
	p := &pspWire{}
	a := newAuthorityWorld(t, func(w *world) {
		w.clock.Advance(-30 * 24 * time.Hour)
		p.provider = newProviderLedger(t, w)
		server := httptest.NewServer(http.HandlerFunc(p.serve))
		t.Cleanup(server.Close)
		target, err := url.Parse(server.URL)
		require.NoError(t, err)
		transport := http.DefaultTransport.(*http.Transport).Clone()
		t.Cleanup(transport.CloseIdleConnections)
		w.deps.StripeTransport = loopbackWire{target: target, wire: transport}
		w.cfg.ProviderWriteMode = openrails.ProviderWritesFull
		w.cfg.Vault = vault.Config()
		w.deps.Vault = vault.Client()
		for slug, declaration := range w.cfg.Merchants {
			declaration.DisplayName = ""
			w.cfg.Merchants[slug] = declaration
		}
		for _, change := range configure {
			change(w)
		}
	})
	return a, p
}

func pspInput() billing.CreatePSPParams {
	return billing.CreatePSPParams{Key: "stripe", Rail: billing.RailStripe, Environment: "test",
		PSPAccount: billing.PSPAccount{AccountID: "acct_e2e"}, PublishableKey: "pk_test_critical",
		Credentials: map[string]string{"secret_key": "sk_test_critical", "webhook_signing_secret": "whsec_critical_fixture"}}
}

func assertNoPSPSecrets(t *testing.T, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	for _, secret := range []string{"sk_test_critical", "sk_test_rotated", "whsec_critical_fixture"} {
		require.NotContains(t, string(raw), secret)
	}
}

func pspRefusal(t *testing.T, err error, status int, code string) {
	t.Helper()
	var refusal *billing.StatusError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, status, refusal.Status)
	require.Equal(t, code, refusal.Code)
}

func TestPSPControls(t *testing.T) {
	t.Run("OR-J06-S01/routing-binds-the-verified-account-and-currency", func(t *testing.T) {
		a, p := newPSPWorld(t)
		psp, err := a.alphaRemote.CreatePSP(t.Context(), pspInput())
		require.NoError(t, err)
		require.Equal(t, "acct_e2e", psp.AccountID)
		require.Equal(t, "test", psp.Environment)
		require.True(t, psp.Credentials["secret_key"].Configured)
		require.NotNil(t, psp.Credentials["secret_key"].ValidatedAt)
		input := saleInput(t, a, p.provider)
		preview, err := a.alphaRemote.PreviewPSPRouting(t.Context(), billing.PreviewPSPRoutingParams{PriceID: input.Lines[0].PriceID, PSP: "stripe"})
		require.NoError(t, err)
		require.Equal(t, ptr("stripe"), preview.PSP)
		require.Equal(t, ptr(billing.RailStripe), preview.Rail)
		require.Empty(t, p.provider.effects(t), "a routing preview must not debit")
		_, err = a.betaRemote.GetPSP(t.Context(), psp.ID)
		var hidden *billing.StatusError
		require.ErrorAs(t, err, &hidden)
		require.Equal(t, http.StatusNotFound, hidden.Status)
		status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "verified-processor"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var order billing.Order
		require.NoError(t, json.Unmarshal(raw, &order))
		require.Equal(t, billing.OrderComplete, order.Status)
		payment, err := a.alpha.GetPayment(t.Context(), *order.Payment.PaymentID)
		require.NoError(t, err)
		require.Equal(t, "USD", payment.Currency)
		require.EqualValues(t, 30_000_000, payment.Amount)
		effects := p.provider.effects(t)
		require.Len(t, effects, 1)
		require.Equal(t, "usd", effects[0].Currency)
		require.EqualValues(t, 3000, effects[0].Amount)
		assertNoPSPSecrets(t, psp)
	})
	t.Run("OR-J06-S02/invalid-or-unarmed-credentials-cannot-enable-purchases", func(t *testing.T) {
		a, p := newPSPWorld(t)
		for _, profile := range []string{"environment", "credential-environment", "provider-environment", "account", "secret"} {
			t.Run(profile, func(t *testing.T) {
				in := pspInput()
				switch profile {
				case "environment":
					in.Environment = "live"
				case "credential-environment":
					in.Credentials["secret_key"] = "sk_live_fixture"
				case "provider-environment":
					p.mu.Lock()
					p.liveBalance = true
					p.mu.Unlock()
					defer func() { p.mu.Lock(); p.liveBalance = false; p.mu.Unlock() }()
				case "account":
					in.AccountID = "acct_foreign"
				case "secret":
					in.Credentials["secret_key"] = "sk_test_rejected"
				}
				_, err := a.alphaRemote.CreatePSP(t.Context(), in)
				if profile == "environment" {
					pspRefusal(t, err, 409, "psp_environment_refused")
				} else {
					pspRefusal(t, err, 400, "psp_credentials_rejected")
				}
				require.NotContains(t, err.Error(), in.Credentials["secret_key"])
				psps, err := a.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
				require.NoError(t, err)
				require.Empty(t, psps.Items)
				require.Empty(t, p.provider.effects(t))
			})
		}
		declared, err := a.local.DeclarePSP(t.Context(), billing.PSPDeclaration{Key: "stripe", Rail: billing.RailStripe, Environment: "test", PSPAccount: billing.PSPAccount{AccountID: "acct_e2e"}})
		require.NoError(t, err)
		require.False(t, declared.Credentials["secret_key"].Configured)
		input := saleInput(t, a, p.provider)
		status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "unarmed-processor"})
		require.Equal(t, http.StatusUnprocessableEntity, status, string(raw))
		require.Contains(t, string(raw), `"code":"payment_option_unavailable"`)
		require.Empty(t, p.provider.effects(t))
		_, err = a.alphaRemote.CreatePSP(t.Context(), pspInput())
		require.NoError(t, err)
		status, raw = a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "armed-processor"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		require.Len(t, p.provider.effects(t), 1)
	})
	t.Run("OR-J06-S03", func(t *testing.T) {
		for _, mode := range []string{"readonly", "limited"} {
			t.Run(mode, func(t *testing.T) {
				a, p := newPSPWorld(t, func(w *world) {
					if mode == "readonly" {
						w.cfg.ProviderWriteMode = openrails.ProviderWritesReadOnly
					} else {
						w.cfg.ProviderWriteMode = openrails.ProviderWritesLimited
					}
				})
				_, err := a.alphaRemote.CreatePSP(t.Context(), pspInput())
				require.NoError(t, err, "read-only credential probes are allowed in both postures")
				if mode == "readonly" {
					input := saleInput(t, a, p.provider)
					status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "readonly-purchase"})
					require.Equal(t, http.StatusServiceUnavailable, status, string(raw))
					require.Contains(t, string(raw), `"code":"provider_writes_disabled"`)
					require.Empty(t, p.provider.effects(t))
					return
				}
				product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "limited-membership", DisplayName: "Limited membership"})
				require.NoError(t, err)
				price, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, UnitAmount: 10_000_000, Currency: "USD", BillingIntervalHours: ptr(24), AccessDurationHours: ptr(24)})
				require.NoError(t, err)
				sub := membershipAtPrice(t, a, p.provider, price.ID, a.aliceToken, "limited-customer-purchase")
				require.Len(t, p.provider.effects(t), 1, "limited allows a customer's own purchase")
				require.EqualValues(t, 1000, p.provider.effects(t)[0].Amount)
				a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()))
				fleet := startFleet(t, a.world)
				runRegistered(t, fleet, "openrails.dunning", 2)
				current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
				require.NoError(t, err)
				require.Equal(t, sub.CurrentPeriodEndsAt, current.CurrentPeriodEndsAt)
				require.Len(t, p.provider.effects(t), 1, "automatic renewal must not debit in limited mode")
			})
		}
	})
	t.Run("OR-J06-S04/rotation-and-archive-preserve-identity-without-exposing-secrets", func(t *testing.T) {
		a, p := newPSPWorld(t)
		initial, err := a.alphaRemote.CreatePSP(t.Context(), pspInput())
		require.NoError(t, err)
		replica, err := openrails.New(t.Context(), a.cfg, a.deps)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, replica.Close(context.Background())) })
		other, err := replica.Merchant(a.alpha.MerchantID())
		require.NoError(t, err)
		cached, err := other.GetPSP(t.Context(), initial.ID)
		require.NoError(t, err)
		require.Equal(t, initial.Revision, cached.Revision)
		// Observe listener readiness only; all configuration mutation and cache
		// readback below use public clients. Do not wait for the 30s fallback.
		require.Eventually(t, func() bool {
			var listeners int
			err := a.pool.QueryRow(t.Context(), "SELECT count(*) FROM pg_stat_activity WHERE query = $1", "LISTEN \""+merchantdocs.Channel(a.schema)+"\"").Scan(&listeners)
			return err == nil && listeners >= 2
		}, 3*time.Second, 10*time.Millisecond)
		cachedAt := time.Now()
		rotated, err := a.alphaRemote.UpdatePSP(t.Context(), initial.ID, billing.UpdatePSPParams{ExpectedRevision: &initial.Revision, Credentials: map[string]string{"secret_key": "sk_test_rotated"}})
		require.NoError(t, err)
		require.Equal(t, initial.ID, rotated.ID)
		require.Equal(t, "acct_e2e", rotated.AccountID)
		require.Greater(t, rotated.Revision, initial.Revision)
		require.Eventually(t, func() bool {
			seen, err := other.GetPSP(t.Context(), initial.ID)
			return err == nil && seen.Revision == rotated.Revision
		}, 3*time.Second, 10*time.Millisecond, "another replica must invalidate its cached credential immediately")
		require.Less(t, time.Since(cachedAt), 30*time.Second, "the periodic cache fallback is not notification proof")
		_, err = a.alphaRemote.UpdatePSP(t.Context(), initial.ID, billing.UpdatePSPParams{ExpectedRevision: &initial.Revision, Credentials: map[string]string{"secret_key": "sk_test_critical"}})
		pspRefusal(t, err, 409, "revision_mismatch")
		p.mu.Lock()
		p.keys = nil
		p.mu.Unlock()
		input := saleInput(t, a, p.provider)
		status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "rotated-processor"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		p.mu.Lock()
		keys := append([]string(nil), p.keys...)
		p.mu.Unlock()
		require.Contains(t, keys, "sk_test_rotated")
		require.NotContains(t, keys, "sk_test_critical")
		_, err = a.alphaRemote.UpdatePSP(t.Context(), initial.ID, billing.UpdatePSPParams{Archived: true})
		pspRefusal(t, err, 409, "psp_last_active")
		archived, err := a.alphaRemote.UpdatePSP(t.Context(), initial.ID, billing.UpdatePSPParams{Archived: true, AllowLast: true})
		require.NoError(t, err)
		require.True(t, archived.Archived)
		require.Equal(t, initial.ID, archived.ID)
		status, raw = a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "archived-processor"})
		require.Equal(t, http.StatusUnprocessableEntity, status, string(raw))
		require.Contains(t, string(raw), `"code":"payment_option_unavailable"`)
		require.Len(t, p.provider.effects(t), 1)
		rows, err := a.alphaRemote.ListPSPs(t.Context(), billing.PSPListParams{})
		require.NoError(t, err)
		assertNoPSPSecrets(t, rows)
		assertNoPSPSecrets(t, rotated)
		assertNoPSPSecrets(t, archived)
	})
	t.Run("OR-J06-S05/merchant-urls-and-provider-redirects-cannot-reach-private-peers", func(t *testing.T) {
		a, p := newPSPWorld(t)
		valid, err := a.alphaRemote.CreateAlertWebhook(t.Context(), billing.CreateAlertWebhookParams{Name: "public-control", URL: "https://alerts.invalid/private-hook-token"})
		require.NoError(t, err)
		require.Equal(t, "alerts.invalid", valid.DestinationHost)
		encoded, err := json.Marshal(valid)
		require.NoError(t, err)
		require.NotContains(t, string(encoded), "private-hook-token")
		for _, destination := range []string{"http://127.0.0.1/private", "http://169.254.169.254/latest/meta-data", "http://[::1]/private", "file:///etc/passwd"} {
			_, err := a.alphaRemote.CreateAlertWebhook(t.Context(), billing.CreateAlertWebhookParams{Name: "blocked", URL: destination})
			pspRefusal(t, err, 400, "webhook_invalid")
		}
		hooks, err := a.alphaRemote.ListAlertWebhooks(t.Context(), billing.AlertWebhookListParams{})
		require.NoError(t, err)
		require.Len(t, hooks.Items, 1)
		require.Equal(t, valid.ID, hooks.Items[0].ID)
		var received atomic.Int32
		private := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received.Add(1); w.WriteHeader(http.StatusNoContent) }))
		t.Cleanup(private.Close)
		address, err := url.Parse(private.URL)
		require.NoError(t, err)
		p.mu.Lock()
		p.redirect = private.URL + "/secret-destination"
		p.mu.Unlock()
		_, err = a.alphaRemote.CreatePSP(t.Context(), pspInput())
		pspRefusal(t, err, 503, "service_unavailable")
		require.NotContains(t, err.Error(), "secret-destination")
		require.Zero(t, received.Load())
		// Allow only this fixture's entry address. A redirect to a different
		// private address must still be rejected by the real redirect policy.
		entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/control" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			http.Redirect(w, r, "http://127.0.0.2:"+address.Port()+"/private", http.StatusFound)
		}))
		t.Cleanup(entry.Close)
		redirectClient := (httpx.Policy{Allow: func(ip net.IP) bool { return ip.Equal(net.ParseIP("127.0.0.1")) }}).Client(time.Second)
		t.Cleanup(redirectClient.CloseIdleConnections)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, entry.URL+"/control", nil)
		require.NoError(t, err)
		response, err := redirectClient.Do(request)
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, response.StatusCode)
		require.NoError(t, response.Body.Close())
		request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, entry.URL+"/redirect", nil)
		require.NoError(t, err)
		response, err = redirectClient.Do(request)
		if response != nil {
			response.Body.Close()
		}
		require.ErrorIs(t, err, httpx.ErrBlockedAddress)
		require.Zero(t, received.Load())
		// Small independent socket-boundary check: the actual guarded dialer
		// must reject a hostname resolving to loopback, not only literal URLs.
		guarded := (httpx.Policy{}).Client(time.Second)
		t.Cleanup(guarded.CloseIdleConnections)
		request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost:"+address.Port()+"/private", nil)
		require.NoError(t, err)
		response, err = guarded.Do(request)
		if response != nil {
			response.Body.Close()
		}
		require.ErrorIs(t, err, httpx.ErrBlockedAddress)
		require.Zero(t, received.Load())
		for _, ip := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "::ffff:127.0.0.1", "fc00::1"} {
			require.True(t, httpx.IsBlockedIP(net.ParseIP(ip)), ip)
		}
		require.False(t, httpx.IsBlockedIP(net.ParseIP("8.8.8.8")))
	})
	t.Run("OR-J06-S30/a-declared-stripe-psp-registers-its-webhook-endpoint", func(t *testing.T) {
		a, p := newPSPWorld(t, func(w *world) { w.cfg.PublicBillingBaseURL = stripeWebhookBase })
		fleet := hostFleet(t, a.world)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		_, err := a.alphaRemote.CreatePSP(t.Context(), pspInput())
		require.NoError(t, err)
		require.Eventually(t, func() bool { return len(p.provider.core.WebhookEndpoints("acct_e2e")) == 1 }, 20*time.Second, 20*time.Millisecond,
			"a new Stripe PSP's endpoint is registered when it is declared, not at the next hourly pass")
		endpoint := p.provider.core.WebhookEndpoints("acct_e2e")[0]
		require.Equal(t, stripeWebhookBase+"/v1/webhooks/stripe/acct_e2e", endpoint["url"])
		require.Equal(t, stripeapi.APIVersion, endpoint["api_version"])
		require.Equal(t, strings.Join(webhooks.HandledStripeEventTypes, ","), endpoint["enabled_events"])
	})
	t.Run("OR-J06-S31/a-dashboard-endpoint-of-a-file-declared-psp-is-managed-not-refused", func(t *testing.T) {
		a, p := newPaymentWorld(t, func(w *world) { w.cfg.PublicBillingBaseURL = stripeWebhookBase })
		url := stripeWebhookBase + "/v1/webhooks/stripe/acct_e2e"
		id := p.core.SeedWebhookEndpoint("acct_e2e", url, stripeapi.APIVersion, []string{"invoice.paid"})
		fleet := hostFleet(t, a.world)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		runRegistered(t, fleet, "openrails.stripe_webhook_reconcile", 1)
		endpoints := p.core.WebhookEndpoints("acct_e2e")
		require.Len(t, endpoints, 1, "a file-declared PSP never mints an endpoint it cannot keep the secret of")
		require.Equal(t, id, endpoints[0]["id"])
		require.Equal(t, "true", endpoints[0]["metadata"].(map[string]string)["openrails_managed"])
		require.Equal(t, strings.Join(webhooks.HandledStripeEventTypes, ","), endpoints[0]["enabled_events"])
	})
}

// stripeWebhookBase is a public https billing mount Stripe could reach.
const stripeWebhookBase = "https://billing.example.test"
