//go:build greenfield && integration

package subscriptions_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// callAt sends one customer request with token to a mounted server.
func (w *world) callAt(server, token, method, path, key string, body any) (int, map[string]any) {
	w.t.Helper()
	var data io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(w.t, err)
		data = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(w.t.Context(), method, server+mountPrefix+"/v1/me"+path, data)
	require.NoError(w.t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(w.t, err)
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	require.NoError(w.t, err)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, out
}

// finitePass is a one-time 30-day pass.
func (w *world) finitePass(entitlement string) *openrails.Price {
	w.t.Helper()
	client := w.client[embedded]
	product, err := client.Products.Create(w.t.Context(), &openrails.ProductCreateParams{Key: "pass-" + uuid.NewString()[:8], DisplayName: "Pass", EntitlementsSpec: map[string]*int{entitlement: nil}})
	require.NoError(w.t, err)
	hours := monthHours
	price, err := client.Prices.Create(w.t.Context(), &openrails.PriceCreateParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 4_990_000, Currency: "USD", AccessDurationHours: &hours})
	require.NoError(w.t, err)
	return price
}

func (c *customer) buyWith(rail, method string, price *openrails.Price, kind openrails.OfferKind, entitlement string) {
	c.w.t.Helper()
	_, err := c.w.client[embedded].CreateCheckoutSession(c.w.t.Context(), openrails.CreateCheckoutSessionRequest{
		OfferKind: kind, Customer: openrails.CheckoutCustomerIdentity{ID: c.id}, Entitlement: entitlement, PriceID: price.ID,
		IdempotencyKey: "buy-" + uuid.NewString(), PaymentOptions: openrails.CheckoutPaymentOptions{PSPID: c.w.psp[rail], Rail: rail, PaymentMethodID: method},
		SuccessURL: "https://greenfield.test/return", CancelURL: "https://greenfield.test/return",
	})
	require.NoError(c.w.t, err)
	c.w.settle()
}

func (c *customer) entitledAt(entitlement string, at time.Time) bool {
	c.w.t.Helper()
	got, err := c.w.client[embedded].CheckEntitlements(c.w.t.Context(), c.id, []string{entitlement}, at)
	require.NoError(c.w.t, err)
	return got[entitlement]
}

// SEC-25: revoked access stays revoked. A refund that revokes a stacked,
// not-yet-started pass, and a merchant revoking a future grant, must not be
// undone by convergence repairing "missing" grant effects.
func TestSecurityRevokedAccessStaysRevoked(t *testing.T) {
	t.Parallel()
	for _, rail := range rails {
		t.Run(rail, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			price := w.finitePass("content:pass")
			c := w.newCustomer()
			method := c.saveCard(rail, visa)
			start := w.clock.Now()
			c.buyWith(rail, method, price, openrails.OfferFinite, "content:pass")
			first := completed(w.payments(embedded, c.id))
			require.Len(t, first, 1)
			w.advance(time.Hour)
			c.buyWith(rail, method, price, openrails.OfferFinite, "content:pass")
			paid := completed(w.payments(embedded, c.id))
			require.Len(t, paid, 2)
			require.True(t, c.entitledAt("content:pass", start.Add(45*day)), "the second pass is stacked after the first")
			second := paid[0]
			if second.ID == first[0].ID {
				second = paid[1]
			}
			_, err := w.client[embedded].RefundPayment(t.Context(), second.ID, openrails.RefundPaymentParams{Full: true, Reason: "requested_by_customer", RevokeAccess: true, IdempotencyKey: "refund-" + second.ID.String()})
			require.NoError(t, err)
			w.settle()
			for range 2 {
				require.False(t, c.entitledAt("content:pass", start.Add(45*day)), "the refunded pass grants nothing")
				require.True(t, c.entitledAt("content:pass", start.Add(15*day)), "the first pass is untouched")
				w.converge()
			}
		})
	}
	t.Run("merchant revoke", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		c := w.newCustomer()
		hours := 24
		client := w.client[embedded]
		_, err := client.GrantEntitlement(t.Context(), c.id, openrails.GrantEntitlementRequest{Entitlement: "content:gift", Hours: &hours})
		require.NoError(t, err)
		future, err := client.GrantEntitlement(t.Context(), c.id, openrails.GrantEntitlementRequest{Entitlement: "content:gift", Hours: &hours})
		require.NoError(t, err)
		now := w.clock.Now()
		require.True(t, c.entitledAt("content:gift", now.Add(30*time.Hour)))
		require.NoError(t, client.RevokeEntitlement(t.Context(), c.id, future.ID))
		for range 2 {
			require.False(t, c.entitledAt("content:gift", now.Add(30*time.Hour)), "the revoked grant stays revoked")
			require.True(t, c.entitledAt("content:gift", now.Add(12*time.Hour)))
			w.converge()
		}
	})
}

// SEC-26: two upgrades of one membership racing on two replicas, to two
// different tiers, charge at most once and leave one membership.
func TestSecurityConcurrentUpgradesChargeOnce(t *testing.T) {
	t.Parallel()
	forEachRail(t, func(t *testing.T, rail string) {
		w := newWorld(t)
		replica := w.sibling()
		group := "g" + uuid.NewString()[:8]
		basic := w.tierPrice(group, 1, 1000, monthHours, false)
		plus := w.tierPrice(group, 2, 2000, monthHours, false)
		pro := w.tierPrice(group, 3, 3000, monthHours, false)
		c, sub := w.engineMember(rail, embedded, basic)
		w.advance(w.subscription(embedded, sub).CurrentPeriodEndsAt.Sub(w.clock.Now()) / 2)
		charges := len(w.railLedger(rail))
		g := w.chargeGate(rail)
		var wg sync.WaitGroup
		change := func(client *openrails.Client, target tier) {
			defer wg.Done()
			_, err := client.ChangeTier(context.WithoutCancel(t.Context()), sub, "upgrade-"+uuid.NewString(), openrails.ChangeTierRequest{PriceID: target.ID})
			t.Logf("upgrade to %s: %v", target.ent, err)
		}
		wg.Add(1)
		go change(w.client[embedded], plus)
		select {
		case <-g.arrived:
		case <-time.After(20 * time.Second):
			t.Fatal("the first upgrade never reached the provider")
		}
		wg.Add(2)
		go change(replica.client, pro)
		go change(w.client[remote], pro)
		time.Sleep(500 * time.Millisecond)
		close(g.release)
		wg.Wait()
		w.stripe.unhold()
		w.nmi.unhold()
		w.settle()
		require.Len(t, w.railLedger(rail), charges+1, "one upgrade charge")
		subs, err := w.client[embedded].ListSubscriptions(t.Context(), openrails.SubscriptionFilter{CustomerID: c.id})
		require.NoError(t, err)
		live := 0
		for _, s := range subs.Data {
			if s.Status == "active" {
				live++
				require.Equal(t, plus.ID, s.PriceID)
			}
		}
		require.Equal(t, 1, live, "one live membership")
		require.True(t, c.entitled(plus.ent))
		require.False(t, c.entitled(pro.ent))
	})
}

// SEC-26: a tier change stays inside one declared tier group. Moving to or
// from an ungrouped product is refused before anything is charged.
func TestSecurityTierChangeStaysInGroup(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	group := "g" + uuid.NewString()[:8]
	basic := w.tierPrice(group, 1, 1000, monthHours, false)
	loose := w.membership("content:loose", 30_000_000)
	c, sub := w.engineMember("nmi", embedded, basic)
	other := w.newCustomer()
	looseSub := other.subscribe(embedded, "nmi", loose.ID, "content:loose", other.saveCard("nmi", visa))
	charges := len(w.railLedger("nmi"))
	for _, tc := range []struct {
		sub    openrails.SubscriptionID
		target string
	}{{sub, loose.ID}, {looseSub, basic.ID}} {
		for _, tp := range []topology{embedded, remote} {
			_, err := w.client[tp].ChangeTier(t.Context(), tc.sub, "cross-"+uuid.NewString(), openrails.ChangeTierRequest{PriceID: tc.target})
			require.Error(t, err)
			_, err = w.client[tp].PreviewTierChange(t.Context(), tc.sub, openrails.ChangeTierRequest{PriceID: tc.target})
			require.Error(t, err)
		}
	}
	w.settle()
	require.Len(t, w.railLedger("nmi"), charges)
	require.Equal(t, "active", w.subscription(embedded, sub).Status)
	require.False(t, c.entitled("content:loose"))
}

// SEC-27: a customer's automation credential is not the customer. It cannot
// start a charge on their saved card; their interactive session can.
func TestSecurityAutomationCredentialCannotCharge(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	// A host's own customer authenticator classifies credentials; "auto-"
	// subjects are the customer's API automation.
	merchantID := w.client[embedded].MerchantID().String()
	hostAuth := billingauth.DelegatedAuthenticatorFunc(func(ctx context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
		p, err := w.auth.AuthenticateRequest(ctx, r)
		if err != nil {
			return nil, err
		}
		subject, class := p.Identity().Subject, billingauth.CredentialClassUserSession
		if id, ok := strings.CutPrefix(subject, "auto-"); ok {
			subject, class = id, billingauth.CredentialClassAutomation
		}
		return &billingauth.DelegatedPrincipal{MerchantID: merchantID, MerchantSlug: w.slug, SubjectID: subject, CredentialClass: class, Issuer: issuer}, nil
	})
	self := w.peer(w.slug, embed.CustomerSelfService, w.declaredPSPs(), hostAuth)
	group := "g" + uuid.NewString()[:8]
	basic := w.tierPrice(group, 1, 1000, monthHours, false)
	plus := w.tierPrice(group, 2, 2000, monthHours, false)
	c, sub := w.engineMember("nmi", embedded, basic)
	method := w.subscription(embedded, sub).PaymentMethodID.String()
	post := w.finitePass("content:post")
	bot := w.auth.token(t, "auto-"+c.id)
	charges := len(w.railLedger("nmi"))

	status, body := w.callAt(self.server.URL, bot, http.MethodPost, "/subscriptions/"+sub.String()+"/change-tier", "bot-"+uuid.NewString(), map[string]any{"price_id": plus.ID})
	require.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, status, "%v", body)
	status, body = w.callAt(self.server.URL, bot, http.MethodPost, "/checkout", "bot-"+uuid.NewString(), map[string]any{
		"price_id": post.ID, "entitlement": "content:post", "offer_kind": "finite",
		"payment": map[string]any{"rail": "nmi", "payment_method_id": method},
	})
	require.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, status, "%v", body)
	w.settle()
	require.Len(t, w.railLedger("nmi"), charges, "no charge from an automation credential")
	require.False(t, c.entitled("content:post"))

	status, body = w.callAt(self.server.URL, c.token, http.MethodPost, "/subscriptions/"+sub.String()+"/change-tier", "user-"+uuid.NewString(), map[string]any{"price_id": plus.ID})
	require.Less(t, status, 300, "%v", body)
	w.settle()
	require.Len(t, w.railLedger("nmi"), charges+1, "the customer's own session upgrades")
}

// SEC-28: resolving a finding executes its recommendation. override_params
// may tune it, never retarget it at another payment or subscription.
func TestSecurityFindingOverrideCannotRetarget(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.armDestructive()
	subject, victim := enroll(t, w, "nmi", embedded), enroll(t, w, "nmi", embedded)
	payment := completed(w.payments(embedded, victim.c.id))[0]
	evidence, err := json.Marshal(map[string]any{"recommendation": map[string]any{"action": "cancel_and_refund", "params": map[string]any{"subscription_id": subject.sub.String()}}})
	require.NoError(t, err)
	var finding string
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`INSERT INTO openrails.reconciliation_findings (merchant_id, finding_type, subject_key, severity, status, evidence)
		SELECT id, 'consistency.security.override', $2, 'critical', 'requires_review', $3::jsonb FROM openrails.merchants WHERE slug = $1 RETURNING id::text`), w.slug, "override-"+uuid.NewString(), string(evidence)).Scan(&finding))

	for _, override := range []map[string]any{
		{"refund_payment_id": payment.ID.String()},
		{"subscription_id": victim.sub.String()},
		{"customer_id": victim.c.id},
	} {
		status, body := w.staffJSON(http.MethodPost, "/v1/merchant/findings/"+finding+"/resolve", map[string]any{"outcome": "approve", "notes": "x", "override_params": override})
		require.Equal(t, http.StatusBadRequest, status, "%v %v", override, body)
	}
	w.settle()
	got, err := w.client[embedded].GetPayment(t.Context(), payment.ID)
	require.NoError(t, err)
	require.False(t, got.Refunded)
	require.Equal(t, "active", w.subscription(embedded, victim.sub).Status)
	for _, entry := range victim.providerLedger() {
		require.Zero(t, entry.Refunded)
	}
}

// SEC-31/32: provider configuration cannot reach a browser or a live
// account unsafely. An injected Stripe transport never exempts a live key
// from sandbox posture, and the Collect.js script served to browsers is only
// ever NMI's.
func TestSecurityProviderConfigurationSafety(t *testing.T) {
	t.Parallel()
	w := newWorld(t)

	t.Run("live Stripe key through an injected transport", func(t *testing.T) {
		recorder := newStripeFake()
		slug := "live-" + uuid.NewString()[:8]
		rt, err := embed.New(t.Context(), embed.Options{
			Merchant: &embed.MerchantDeclaration{Slug: slug, Config: embed.MerchantConfig{DisplayName: slug, PSPs: map[string]embed.PSPConfig{
				"stripe": {"stripe": {AccountID: "acct_live_probe", Secrets: map[string]string{"secret_key": "sk_live_greenfield", "webhook_signing_secret": "whsec_live"}}},
			}}},
			Config: &config.Config{TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeFull, AllowCatalogUpdates: true,
				DB: &config.DBConfig{URL: w.dsn, Schema: w.schema}},
			PGXPool: w.pool, River: embed.RiverFromHost(), StripeTransport: recorder, Clock: w.clock,
		})
		if err != nil {
			t.Logf("refused at construction: %v", err)
			return
		}
		t.Cleanup(func() { _ = rt.Close(context.Background()) })
		client, err := rt.Client()
		require.NoError(t, err)
		product, err := client.Products.Create(t.Context(), &openrails.ProductCreateParams{Key: "live-" + uuid.NewString()[:8], DisplayName: "Live", EntitlementsSpec: map[string]*int{"content:live": nil}})
		require.NoError(t, err)
		price, err := client.Prices.Create(t.Context(), &openrails.PriceCreateParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 1_000_000, Currency: "USD"})
		require.NoError(t, err)
		_, err = client.CreateCheckoutSession(t.Context(), openrails.CreateCheckoutSessionRequest{
			Customer: openrails.CheckoutCustomerIdentity{ID: uuid.NewString(), VerifiedEmail: "live@example.test"}, PriceID: price.ID, Entitlement: "content:live",
			OfferKind: openrails.OfferPermanent, PaymentOptions: openrails.CheckoutPaymentOptions{Rail: "stripe"}, IdempotencyKey: "live-" + uuid.NewString(),
			SuccessURL: "https://greenfield.test/return", CancelURL: "https://greenfield.test/return",
		})
		require.Error(t, err, "a live key is disarmed in a sandbox deployment")
		recorder.mu.Lock()
		defer recorder.mu.Unlock()
		for _, call := range recorder.writes {
			require.NotEqual(t, http.MethodPost, call.Method, "no provider mutation with a live key: %s", call.Path)
		}
	})

	t.Run("merchant-configured Collect.js origin", func(t *testing.T) {
		psps := map[string]embed.PSPConfig{"nmi": {"nmi": {AccountID: "script-nmi", Secrets: map[string]string{"security_key": "script-nmi-key", "webhook_signing_secret": "script-whsec"},
			Settings: map[string]any{"tokenization_key": "script-tokenization", "tokenization_url": "https://evil.example/token/Collect.js"}}}}
		r := w.peer("script-"+uuid.NewString()[:8], embed.CustomerBillingManagement, psps)
		cfg, err := r.client.GetCheckoutConfig(t.Context())
		require.NoError(t, err)
		found := false
		for _, psp := range cfg.PSPs {
			if psp.Rail == "nmi" {
				found = true
				require.Equal(t, "https://secure.networkmerchants.com/token/Collect.js", psp.Config["tokenization_url"])
			}
		}
		require.True(t, found, "%+v", cfg)
	})
}
