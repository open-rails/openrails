//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
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
func (w *world) finitePass(entitlement string) *billing.Price {
	w.t.Helper()
	client := w.client[embedded]
	product, err := client.CreateProduct(w.t.Context(), billing.CreateProductParams{Key: "pass-" + uuid.NewString()[:8], DisplayName: "Pass", Entitlements: []string{entitlement}})
	require.NoError(w.t, err)
	hours := monthHours
	price, err := client.CreatePrice(w.t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 4_990_000, Currency: "USD", AccessDurationHours: &hours})
	require.NoError(w.t, err)
	return price
}

// buyWith pays a session for price with a card saved on rail.
func (c *customer) buyWith(rail, method string, price *billing.Price) {
	c.w.t.Helper()
	c.mustCheckout(embedded, order{price: price.ID, rail: rail, method: method, successURL: "https://e2e.test/return"})
}

func (c *customer) entitledAt(entitlement string, at time.Time) bool {
	c.w.t.Helper()
	got, err := heldKeys(c.w.t.Context(), c.w.client[embedded], c.customerID(), at, entitlement)
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
			c.buyWith(rail, method, price)
			first := completed(w.payments(embedded, c.id))
			require.Len(t, first, 1)
			w.advance(time.Hour)
			c.buyWith(rail, method, price)
			paid := completed(w.payments(embedded, c.id))
			require.Len(t, paid, 2)
			require.True(t, c.entitledAt("content:pass", start.Add(45*day)), "the second pass is stacked after the first")
			second := paid[0]
			if second.ID == first[0].ID {
				second = paid[1]
			}
			_, err := w.client[embedded].RefundPayment(t.Context(), second.ID, billing.RefundPaymentParams{Full: true, Reason: "requested_by_customer", RevokeAccess: true, IdempotencyKey: "refund-" + second.ID.String()})
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
		gift := w.giftProduct("content:gift")
		c.grant(gift, &hours, nil)
		future := c.grant(gift, &hours, nil)
		now := w.clock.Now()
		require.True(t, c.entitledAt("content:gift", now.Add(30*time.Hour)), "hours extend after the live grant")
		require.NoError(t, w.client[embedded].DeleteProductAccess(t.Context(), c.customerID(), future.ID))
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
		var first, racers sync.WaitGroup
		change := func(wg *sync.WaitGroup, server string, target tier) {
			defer wg.Done()
			_, err := c.changeAt(server, sub, billing.ChangeSubscriptionParams{PriceID: priceRef(target.ID), IdempotencyKey: "upgrade-" + uuid.NewString()})
			t.Logf("upgrade to %s: %v", target.ent, err)
		}
		first.Add(1)
		go change(&first, w.server.URL, plus)
		select {
		case <-g.arrived:
		case <-time.After(20 * time.Second):
			t.Fatal("the first upgrade never reached the provider")
		}
		racers.Add(2)
		go change(&racers, replica.server.URL, pro)
		go change(&racers, w.server.URL, pro)
		releaseAfterRacers(t, g, &racers)
		first.Wait()
		w.stripe.unhold()
		w.nmi.unhold()
		w.settle()
		require.Len(t, w.railLedger(rail), charges+1, "one upgrade charge")
		subs, err := w.client[embedded].ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: c.customerID()})
		require.NoError(t, err)
		live := 0
		for _, s := range subs.Items {
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
	looseSub := other.subscribe(embedded, "nmi", loose.ID.String(), "content:loose", other.saveCard("nmi", visa))
	charges := len(w.railLedger("nmi"))
	for _, tc := range []struct {
		sub    billing.SubscriptionID
		target billing.PriceID
	}{{sub, loose.ID}, {looseSub, basic.ID}} {
		for _, tp := range []topology{embedded, remote} {
			_, err := w.client[tp].ChangeSubscription(t.Context(), tc.sub, billing.ChangeSubscriptionParams{Reason: "customer asked", PriceID: priceRef(tc.target), IdempotencyKey: "cross-" + uuid.NewString()})
			require.Error(t, err)
			_, err = w.client[tp].PreviewSubscriptionChange(t.Context(), tc.sub, billing.ChangeSubscriptionParams{PriceID: priceRef(tc.target)})
			require.Error(t, err)
		}
	}
	w.settle()
	require.Len(t, w.railLedger("nmi"), charges)
	require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, sub).Status)
	require.False(t, c.entitled("content:loose"))
}

// SEC-27: a customer's automation credential is not the customer. It cannot
// start a charge on their saved card; their interactive session can.
func TestSecurityAutomationCredentialCannotCharge(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	self := w.peer(w.slug, w.auth, w.declaredPSPs())
	group := "g" + uuid.NewString()[:8]
	basic := w.tierPrice(group, 1, 1000, monthHours, false)
	plus := w.tierPrice(group, 2, 2000, monthHours, false)
	c, sub := w.engineMember("nmi", embedded, basic)
	post := w.finitePass("content:post")
	bot := w.auth.apiKeyToken(t, c.id)
	charges := len(w.railLedger("nmi"))

	status, body := w.callAt(self.server.URL, bot, http.MethodPost, "/subscriptions/"+sub.String()+"/change", "bot-"+uuid.NewString(), map[string]any{"price_id": plus.ID})
	require.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, status, "%v", body)
	// An automation credential cannot mint a session that pays with the
	// customer's saved card.
	status, body = w.callAt(self.server.URL, bot, http.MethodPost, "/checkout-sessions", "", map[string]any{"price_id": post.ID})
	require.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, status, "%v", body)
	w.settle()
	require.Len(t, w.railLedger("nmi"), charges, "no charge from an automation credential")
	require.False(t, c.entitled("content:post"))

	status, body = w.callAt(self.server.URL, c.token, http.MethodPost, "/subscriptions/"+sub.String()+"/change", "user-"+uuid.NewString(), map[string]any{"price_id": plus.ID})
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
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`INSERT INTO billing.reconciliation_findings (merchant_id, finding_type, subject_key, severity, status, evidence)
		SELECT id, 'consistency.security.override', $2, 'critical', 'requires_review', $3::jsonb FROM billing.merchants WHERE slug = $1 RETURNING id::text`), w.slug, "override-"+uuid.NewString(), string(evidence)).Scan(&finding))

	for _, override := range []map[string]any{
		{"refund_payment_id": payment.ID.String()},
		{"subscription_id": victim.sub.String()},
		{"customer_id": victim.c.id},
	} {
		status, body := w.staffJSON(http.MethodPost, "/v1/admin/findings/fnd_"+finding+"/resolve", map[string]any{"outcome": "approve", "notes": "x", "override_params": override})
		require.Equal(t, http.StatusBadRequest, status, "%v %v", override, body)
		require.Contains(t, fmt.Sprint(body), "override_params", "refused for its override, not its id")
	}
	w.settle()
	got, err := w.client[embedded].GetPayment(t.Context(), payment.ID)
	require.NoError(t, err)
	require.Zero(t, got.AmountRefunded)
	require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, victim.sub).Status)
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
		rt, err := openrails.New(t.Context(), openrails.Config{
			Database: openrails.DatabaseConfig{Schema: w.schema, RiverSchema: w.schema},
			TestMode: openrails.Sandbox, ProviderWriteMode: openrails.ProviderWritesFull,
			DB:       &openrails.DBConfig{URL: w.dsn},
			Merchant: openrails.MerchantDeclaration{Slug: slug, DisplayName: slug, PSPs: map[string]openrails.PSPConfig{"stripe": {Rail: "stripe", AccountID: "acct_live_probe", Secrets: map[string]string{"secret_key": "sk_live_e2e", "webhook_signing_secret": "whsec_live"}}}},
		}, openrails.Deps{FXTransport: testFX.Transport(), Postgres: w.pool, StripeTransport: recorder, Clock: w.clock})
		if err != nil {
			t.Logf("refused at construction: %v", err)
			return
		}
		t.Cleanup(func() { _ = rt.Close(context.Background()) })
		client := rt
		product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "live-" + uuid.NewString()[:8], DisplayName: "Live", Entitlements: []string{"content:live"}})
		require.NoError(t, err)
		price, err := client.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 1_000_000, Currency: "USD"})
		require.NoError(t, err)
		_, err = client.CreateCheckoutSession(t.Context(), billing.CreateCheckoutSessionParams{
			Customer: billing.CheckoutCustomerIdentity{ID: cid(uuid.NewString()), VerifiedEmail: "live@example.test"}, PriceID: price.ID, SuccessURL: "https://e2e.test/return",
		})
		require.ErrorIs(t, err, billing.ErrInvalid, "a live key is disarmed in a sandbox deployment: nothing sells")
		for _, call := range recorder.Mutations("") {
			require.NotEqual(t, http.MethodPost, call.Method, "no provider mutation with a live key: %s", call.Path)
		}
	})

	t.Run("merchant-configured Collect.js origin", func(t *testing.T) {
		psps := map[string]openrails.PSPConfig{"nmi": {Rail: "nmi", AccountID: "script-nmi", Secrets: map[string]string{"security_key": "script-nmi-key", "webhook_signing_secret": "script-whsec"},
			Settings: map[string]any{"tokenization_key": "script-tokenization", "tokenization_url": "https://evil.example/token/Collect.js"}}}
		r := w.peer("script-"+uuid.NewString()[:8], w.auth, psps)
		cfg, err := r.client.GetPublicConfig(t.Context())
		require.NoError(t, err)
		found := false
		for _, psp := range cfg.Payment.PSPs {
			if psp.Rail == "nmi" {
				found = true
				require.Equal(t, "https://secure.networkmerchants.com/token/Collect.js", psp.Config["tokenization_url"])
			}
		}
		require.True(t, found, "%+v", cfg)
	})
}
