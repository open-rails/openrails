//go:build integration

package critical_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/stretchr/testify/require"
)

// The vendor fixture owns collected intents, durable cards and conversion
// idempotency. The separate NMI journal owns actual gateway debit evidence.
type merchantBT struct {
	mu          sync.Mutex
	server      *httptest.Server
	intents     map[string]basistheory.TokenIntent
	tokens      map[string]basistheory.CardToken
	conversions int
	deletions   int
	gateway     http.RoundTripper
}

const merchantBTTenant = "00000000-0000-4000-8000-000000000031"

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
	b.intents[id] = basistheory.TokenIntent{ID: id, TenantID: tenant, Type: "card", CreatedBy: "owned-collection-app", Fingerprint: "card-4242", Card: &basistheory.CardDetails{Last4: "4242", Brand: "visa", ExpirationMonth: 12, ExpirationYear: 2035}}
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
	t.Run("OR-J12-S05/merchant-owned-vault-keeps-card-and-charge-authority", func(t *testing.T) {
		vendor := newMerchantBT(t)
		a, journal := newNMIWorld(t, func(w *world) {
			vendor.gateway = w.deps.NMITransport
			target, err := url.Parse(vendor.server.URL)
			require.NoError(t, err)
			wire := http.DefaultTransport.(*http.Transport).Clone()
			t.Cleanup(wire.CloseIdleConnections)
			w.deps.BasisTheoryTransport = custodianProbeWire{loopback: loopbackWire{target: target, wire: wire}, wire: wire}
			merchant := w.cfg.Merchants["alpha"]
			merchant.Custodians = openrails.Custodians{"owned": openrails.CustodianConfig{Kind: "basis_theory", Environment: "test", AccountID: merchantBTTenant, Settings: map[string]any{"public_api_key": "public-merchant"}, Secrets: map[string]string{"api_key": "private-merchant"}}}
			psp := merchant.PSPs["nmi"]
			psp.Custodian = "owned"
			merchant.PSPs["nmi"] = psp
			w.cfg.Merchants["alpha"] = merchant
		})
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
		_ = bytes.NewBuffer(nil)
		_ = time.Second
	})
}
