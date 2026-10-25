//go:build integration

package critical_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/vaulttest"
	"github.com/stretchr/testify/require"
)

// The vendor fixture owns collected intents, durable cards and conversion
// idempotency. The separate NMI journal owns actual gateway debit evidence.
type merchantBT struct {
	mu            sync.Mutex
	server        *httptest.Server
	intents       map[string]basistheory.TokenIntent
	tokens        map[string]basistheory.CardToken
	conversions   int
	deletions     int
	dropSaleReply bool
	gateway       http.RoundTripper
}

const merchantBTTenant = "00000000-0000-4000-8000-000000000031"
const merchantBTApplication = "00000000-0000-4000-8000-000000000032"

func newMerchantBT(t *testing.T) *merchantBT {
	b := &merchantBT{intents: map[string]basistheory.TokenIntent{}, tokens: map[string]basistheory.CardToken{}}
	b.server = httptest.NewServer(http.HandlerFunc(b.serve))
	t.Cleanup(b.server.Close)
	return b
}

func (b *merchantBT) collect(tenant string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := uuid.NewString()
	b.intents[id] = basistheory.TokenIntent{ID: id, TenantID: tenant, Type: "card", CreatedBy: merchantBTApplication, Fingerprint: "card-4242", Card: &basistheory.CardDetails{Last4: "4242", Brand: "visa", ExpirationMonth: 12, ExpirationYear: 2035}}
	return id
}

func (b *merchantBT) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("BT-API-KEY") != "private-merchant" {
		http.Error(w, "not authorized", 401)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/proxy" {
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(400)
			return
		}
		ref := r.Form.Get("ccnumber")
		root := strings.Split(ref, " ")
		if len(root) < 4 {
			http.Error(w, "bad token expression", 400)
			return
		}
		id := root[2]
		b.mu.Lock()
		intent, hasIntent := b.intents[id]
		token, hasToken := b.tokens[id]
		gateway := b.gateway
		b.mu.Unlock()
		if (!hasIntent || intent.TenantID != merchantBTTenant) && (!hasToken || token.TenantID != merchantBTTenant) {
			http.Error(w, "token not in this tenant", 403)
			return
		}
		if !strings.Contains(ref, `| json: "$.data.number"`) || !strings.Contains(r.Form.Get("ccexp"), `card_exp: "MMYY"`) {
			http.Error(w, "bad expression", 400)
			return
		}
		r.Form.Set("ccnumber", "4242424242424242")
		r.Form.Set("ccexp", "1235")
		if r.Form.Get("cvv") != "" {
			r.Form.Set("cvv", "123")
		}
		forward, err := http.NewRequestWithContext(r.Context(), http.MethodPost, r.Header.Get("BT-PROXY-URL"), strings.NewReader(r.Form.Encode()))
		if err != nil || gateway == nil {
			http.Error(w, "gateway unavailable", 503)
			return
		}
		forward.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response, err := gateway.RoundTrip(forward)
		if err != nil {
			http.Error(w, "gateway reply lost", 502)
			return
		}
		defer response.Body.Close()
		b.mu.Lock()
		drop := b.dropSaleReply && r.Form.Get("type") == "sale"
		if drop {
			b.dropSaleReply = false
		}
		b.mu.Unlock()
		if drop {
			_, _ = io.Copy(io.Discard, response.Body)
			http.Error(w, "gateway response lost", 502)
			return
		}
		w.Header().Set("BT-PROXY-DESTINATION-STATUS", "200")
		w.Header().Set("Content-Type", "application/x-www-form-urlencoded")
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case r.URL.Path == "/applications/key":
		_ = json.NewEncoder(w).Encode(map[string]string{"tenant_id": merchantBTTenant})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/token-intents/"):
		item, ok := b.intents[strings.TrimPrefix(r.URL.Path, "/token-intents/")]
		if !ok {
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(item)
	case r.Method == http.MethodPost && r.URL.Path == "/tokens":
		var body struct {
			ID         string            `json:"id"`
			Intent     string            `json:"token_intent_id"`
			Containers []string          `json:"containers"`
			Metadata   map[string]string `json:"metadata"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.ID == "" || r.Header.Get("BT-IDEMPOTENCY-KEY") == "" {
			w.WriteHeader(400)
			return
		}
		if saved, ok := b.tokens[body.ID]; ok {
			_ = json.NewEncoder(w).Encode(saved)
			return
		}
		intent, ok := b.intents[body.Intent]
		if !ok {
			w.WriteHeader(404)
			return
		}
		saved := basistheory.CardToken{ID: body.ID, TenantID: intent.TenantID, Type: intent.Type, Fingerprint: intent.Fingerprint, Card: intent.Card, Containers: body.Containers, Metadata: body.Metadata}
		b.tokens[body.ID] = saved
		b.conversions++
		_ = json.NewEncoder(w).Encode(saved)
	case strings.HasPrefix(r.URL.Path, "/tokens/"):
		id := strings.TrimPrefix(r.URL.Path, "/tokens/")
		saved, ok := b.tokens[id]
		if !ok {
			w.WriteHeader(404)
			return
		}
		if r.Method == http.MethodDelete {
			delete(b.tokens, id)
			b.deletions++
			w.WriteHeader(204)
			return
		}
		if r.Method != http.MethodGet {
			w.WriteHeader(405)
			return
		}
		_ = json.NewEncoder(w).Encode(saved)
	default:
		w.WriteHeader(404)
	}
}

func TestMerchantOwnedBasisTheoryCardLifecycle(t *testing.T) {
	for _, profile := range []string{"merchant-owned-vault", "hosted-isolated-vault"} {
		t.Run("OR-J12-S06/"+profile, func(t *testing.T) {
			vendor := newMerchantBT(t)
			t.Setenv(vaulttest.AddrEnv, os.Getenv("OPENRAILS_CRITICAL_VAULT_ADDR"))
			t.Setenv(vaulttest.TokenEnv, os.Getenv("OPENRAILS_CRITICAL_VAULT_TOKEN"))
			vault := vaulttest.New(t)
			a, journal := newNMIWorld(t, func(w *world) {
				w.cfg.Vault = vault.Config()
				w.deps.Vault = vault.Client()
				if profile == "hosted-isolated-vault" {
					w.cfg.CardVault = &openrails.CardVaultConfig{Kind: "basis_theory", TenantID: merchantBTTenant, KVMount: vault.Config().KVMount}
				}
				vendor.gateway = w.deps.NMITransport
				target, err := url.Parse(vendor.server.URL)
				require.NoError(t, err)
				wire := http.DefaultTransport.(*http.Transport).Clone()
				t.Cleanup(wire.CloseIdleConnections)
				w.deps.BasisTheoryTransport = custodianProbeWire{loopback: loopbackWire{target: target, wire: wire}, wire: wire}
				for name, merchant := range w.cfg.Merchants {
					merchant.DisplayName = ""
					merchant.PSPs = nil
					merchant.Custodians = nil
					merchant.CardVault = ""
					w.cfg.Merchants[name] = merchant
				}
			})
			custody, newVault := "owned", "nmi"
			if profile == "hosted-isolated-vault" {
				custody, newVault = "", "platform"
				_, err := a.deps.Vault.Logical().WriteWithContext(t.Context(), a.cfg.Vault.KVMount+"/data/openrails/platform_vault/"+merchantBTTenant, map[string]any{"data": map[string]any{"api_key": "private-merchant", "collection_applications": map[string]any{a.alpha.MerchantID().UUID().String(): map[string]string{"id": merchantBTApplication, "public_api_key": "public-merchant"}}}})
				require.NoError(t, err)
			} else {
				_, err := a.alphaRemote.PutCustodian(t.Context(), "owned", billing.PutCustodianParams{ExpectedRevision: 0, Kind: "basis_theory", Environment: "test", AccountID: merchantBTTenant, Settings: map[string]any{"public_api_key": "public-merchant"}, Credentials: map[string]string{"api_key": "private-merchant"}})
				require.NoError(t, err)
			}
			_, err := a.alphaRemote.CreatePSP(t.Context(), billing.CreatePSPParams{Key: "nmi", Rail: billing.RailNMI, Environment: "test", Custodian: custody, PSPAccount: billing.PSPAccount{GatewayID: "critical-gateway"}, TokenizationKey: "critical-collect-key", Credentials: map[string]string{"security_key": "critical-nmi-key", "webhook_signing_secret": "critical-nmi-webhook"}})
			require.NoError(t, err)
			_, err = a.alphaRemote.UpdateMerchantConfiguration(t.Context(), billing.UpdateMerchantConfigurationParams{MerchantSettings: &billing.MerchantSettings{CardVault: newVault}})
			require.NoError(t, err)
			psps, err := a.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
			require.NoError(t, err)
			require.Len(t, psps.Items, 1)
			collected := vendor.collect(merchantBTTenant)
			input := billing.CreatePaymentMethodParams{PSPID: psps.Items[0].ID, Token: collected}
			status, raw := a.request(t, http.MethodPost, "/v1/me/payment-methods", a.aliceToken, input)
			require.Equal(t, http.StatusCreated, status, string(raw))
			var method billing.PaymentMethod
			require.NoError(t, json.Unmarshal(raw, &method))
			require.Equal(t, billing.PaymentMethodActive, method.Status)
			require.Equal(t, "4242", *method.Card.Last4)
			journal.money(t, 0, 0)
			status, raw = a.request(t, http.MethodPost, "/v1/me/payment-methods", a.aliceToken, input)
			require.Equal(t, 201, status, string(raw))
			var replay billing.PaymentMethod
			require.NoError(t, json.Unmarshal(raw, &replay))
			require.Equal(t, method.ID, replay.ID)
			vendor.mu.Lock()
			conversions := vendor.conversions
			vendor.mu.Unlock()
			require.Equal(t, 1, conversions)
			status, raw = a.request(t, http.MethodPost, "/v1/me/payment-methods", a.bobToken, input)
			require.NotEqual(t, 201, status, string(raw))
			bad := input
			bad.Token = vendor.collect("foreign-vault")
			status, raw = a.request(t, http.MethodPost, "/v1/me/payment-methods", a.aliceToken, bad)
			require.GreaterOrEqual(t, status, 400, string(raw))
			journal.money(t, 0, 0)
			product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "merchant-bt-membership", DisplayName: "Merchant vault membership", Entitlements: []string{"vault:member"}})
			require.NoError(t, err)
			price, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, UnitAmount: 23_000_000, Currency: "USD", BillingIntervalHours: ptr(24)})
			require.NoError(t, err)
			buy := billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price.ID}}, ExpectedTotal: ptr(int64(23_000_000)), Payment: &billing.OrderPaymentParams{PaymentMethodID: &method.ID}}
			vendor.mu.Lock()
			vendor.dropSaleReply = true
			vendor.mu.Unlock()
			status, raw = a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, buy, map[string]string{"Idempotency-Key": "merchant-owned-bt-sale"})
			require.Equal(t, 201, status, string(raw))
			var order billing.Order
			require.NoError(t, json.Unmarshal(raw, &order))
			journal.money(t, 1, 2300)
			status, raw = a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, buy, map[string]string{"Idempotency-Key": "merchant-owned-bt-sale"})
			require.Equal(t, 201, status, string(raw))
			var recovered billing.Order
			require.NoError(t, json.Unmarshal(raw, &recovered))
			require.Equal(t, order.ID, recovered.ID)
			journal.money(t, 1, 2300)
			// Replay preserves the original answer while the ordinary operation
			// worker verifies the lost gateway reply from its independent receipt.
			a.clock.Advance(2 * time.Minute)
			fleet := startFleet(t, a.world)
			runRegistered(t, fleet, "openrails.dunning", 1)
			require.Eventually(t, func() bool {
				current, err := a.alpha.GetOrder(t.Context(), order.ID)
				return err == nil && current.Status == billing.OrderComplete
			}, 15*time.Second, 20*time.Millisecond, "lost proxy response is recovered through exact gateway evidence")
			journal.money(t, 1, 2300)
			subs, err := a.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: method.CustomerID})
			require.NoError(t, err)
			require.Len(t, subs.Items, 1)
			sub := subs.Items[0]
			require.Equal(t, "engine", string(sub.CollectionPolicy))
			require.NotNil(t, sub.CurrentPeriodEndsAt)
			due := *sub.CurrentPeriodEndsAt
			if profile == "hosted-isolated-vault" {
				_, err = a.alpha.UpdateMerchantConfiguration(t.Context(), billing.UpdateMerchantConfigurationParams{MerchantSettings: &billing.MerchantSettings{CardVault: "nmi"}})
			} else {
				_, err = a.alpha.UpdatePSP(t.Context(), psps.Items[0].ID, billing.UpdatePSPParams{Custodian: ptr("")})
			}
			require.NoError(t, err, "changing new-card custody must preserve existing mandates")
			a.clock.Advance(due.Sub(a.clock.Now()))
			runRegistered(t, fleet, "openrails.dunning", 2)
			require.Eventually(t, func() bool {
				current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
				return err == nil && current.CurrentPeriodEndsAt != nil && current.CurrentPeriodEndsAt.After(due)
			}, 15*time.Second, 20*time.Millisecond, "the engine renews with this merchant's own vault and original mandate")
			journal.money(t, 2, 4600)
			runRegistered(t, fleet, "openrails.dunning", 2)
			journal.money(t, 2, 4600)
			status, raw = a.request(t, http.MethodDelete, "/v1/me/payment-methods/"+method.ID.String(), a.aliceToken, nil)
			require.Equal(t, http.StatusConflict, status, string(raw))
			require.Error(t, a.alphaRemote.DeletePaymentMethod(t.Context(), method.CustomerID, method.ID), "merchant removal preserves the same active obligation guard")
			status, raw = a.request(t, http.MethodPost, "/v1/me/subscriptions/"+sub.ID.String()+"/cancel", a.aliceToken, map[string]string{"reason": "Finished"})
			require.Equal(t, 200, status, string(raw))
			require.NoError(t, a.alpha.DeletePaymentMethod(t.Context(), method.CustomerID, method.ID), "a trusted embedded host removes an unused card without impersonating its customer")
			status, raw = a.request(t, http.MethodDelete, "/v1/me/payment-methods/"+method.ID.String(), a.aliceToken, nil)
			require.Equal(t, http.StatusNoContent, status, string(raw))
			vendor.mu.Lock()
			deletions := vendor.deletions
			remaining := len(vendor.tokens)
			vendor.mu.Unlock()
			require.Equal(t, 1, deletions)
			require.Zero(t, remaining)
			journal.money(t, 2, 4600)
			payments, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: method.CustomerID})
			require.NoError(t, err)
			require.Len(t, payments.Items, 2, "removal retains both financial receipts")
		})
	}
}
