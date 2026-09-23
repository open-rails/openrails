//go:build integration

package integrationharness

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	orauthkit "github.com/open-rails/openrails/internal/hostauth"
	embcp "github.com/open-rails/openrails/internal/operator"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/catalog"
	"github.com/stretchr/testify/require"
)

// gatewayDeploymentWire answers the NMI hosts themselves (no loopback
// fixture): the regular gateway's read-only test_mode_status per security
// key, v5 vault create/read, and Direct Post sales via FakeNMIGateway. The
// dedicated sandbox host always answers 401, as it did for the demo account.
type gatewayDeploymentWire struct {
	gateway *FakeNMIGateway

	mu         sync.Mutex
	vaults     map[string]string // vault -> billing id
	testMode   map[string]string // security key -> test_mode_enabled
	modeReads  map[string]int
	vaultAdds  map[string]int
	sandbox    int
	saleHosts  []string
	unexpected []string
}

func (w *gatewayDeploymentWire) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	rec := httptest.NewRecorder()
	w.mu.Lock()
	switch {
	case req.URL.Host == "sandbox.nmi.com":
		w.sandbox++
		w.mu.Unlock()
		http.Error(rec, "unauthorized", http.StatusUnauthorized)
		return rec.Result(), nil
	case req.URL.Host == "secure.nmi.com" && req.URL.Path == "/api/query.php":
		form, _ := url.ParseQuery(string(body))
		if form.Get("report_type") == "test_mode_status" {
			key := form.Get("security_key")
			w.modeReads[key]++
			enabled := w.testMode[key]
			w.mu.Unlock()
			_, _ = io.WriteString(rec, "<nm_response><test_mode_enabled>"+enabled+"</test_mode_enabled></nm_response>")
			return rec.Result(), nil
		}
	case req.URL.Host == "secure.nmi.com" && strings.HasPrefix(req.URL.Path, "/api/v5/customers"):
		vault := strings.TrimPrefix(req.URL.Path, "/api/v5/customers/")
		if req.Method == http.MethodPost && req.URL.Path == "/api/v5/customers" {
			w.vaultAdds[req.Header.Get("Authorization")]++
			vault = "gw-vault-" + uuid.NewString()
			w.vaults[vault] = "gw-billing-" + uuid.NewString()
		}
		billing, found := w.vaults[vault]
		w.mu.Unlock()
		if !found {
			http.Error(rec, "no such vault", http.StatusNotFound)
			return rec.Result(), nil
		}
		rec.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rec).Encode(map[string]any{"object": "customer", "id": vault, "billing": []any{map[string]any{"id": billing, "priority": 1, "payment_details": map[string]any{"card_number": "411111******1111", "card_exp": "1230"}}}})
		return rec.Result(), nil
	case req.URL.Host == "secure.networkmerchants.com" && req.URL.Path == "/api/transact.php":
		w.saleHosts = append(w.saleHosts, req.URL.Host)
	case req.URL.Host == "secure.nmi.com" && req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/api/v5/payments/"):
	default:
		w.unexpected = append(w.unexpected, req.Method+" "+req.URL.String())
		w.mu.Unlock()
		http.Error(rec, "unexpected NMI request", http.StatusNotImplemented)
		return rec.Result(), nil
	}
	w.mu.Unlock()
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w.gateway.serve(rec, req)
	return rec.Result(), nil
}

func (w *gatewayDeploymentWire) counts(t *testing.T, key string) (modeReads, vaultAdds, sandbox int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	require.Empty(t, w.unexpected)
	return w.modeReads[key], w.vaultAdds[key], w.sandbox
}

// TestNMIGatewayDeploymentPSPCardSaveAndOneTimeSale (#1055): a PSP declared
// endpoint_deployment: gateway is verified once on the regular gateway, and
// every client built for it carries that exact identity. Card save reuses the
// verdict and vaults on the gateway; a one-time price declared for NMI needs
// no provider plan, is offered, and sells through the gateway. A PSP whose
// credential reports live refuses the card save before any vault call. The
// dedicated sandbox host is never contacted.
func TestNMIGatewayDeploymentPSPCardSaveAndOneTimeSale(t *testing.T) {
	ctx := t.Context()
	h := New(t, ctx)
	wire := &gatewayDeploymentWire{gateway: NewFakeNMIGateway(t), vaults: map[string]string{}, testMode: map[string]string{}, modeReads: map[string]int{}, vaultAdds: map[string]int{}}
	var host billingauth.DelegatedAuthenticator
	surface := h.StartStandalone("USD", func(c *standaloneConfig) {
		c.delegatedAuthenticator = billingauth.DelegatedAuthenticatorFunc(func(ctx context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
			return host.AuthenticateDelegated(ctx, r)
		})
	})
	rt := surface.App().Runtime
	require.NotNil(t, rt.NMIClients)
	rt.NMIClients.Transport = wire
	cp := embcp.Get(surface.App())

	type merchantFixture struct {
		owned OwnedMerchant
		key   string
		psp   uuid.UUID
		owner *openrails.Client
		token string
		user  string
	}
	arm := func(name, key, testMode string) merchantFixture {
		owned := surface.ProvisionOwnedMerchant(name + "-" + uuid.NewString()[:8])
		account := "gw-" + uuid.NewString()[:8]
		wire.mu.Lock()
		wire.testMode[key] = testMode
		wire.mu.Unlock()
		SeedPSPs(ctx, t, rt, owned.MerchantID, config.PSPSet{name: {Rail: models.RailNMI, AccountID: account, NMI: &config.NMIRailConfig{SecurityKey: key, EndpointDeployment: config.NMIEndpointGateway}}})
		var psp uuid.UUID
		require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT id FROM billing.psps WHERE merchant_id=$1 AND account_id=$2`, owned.MerchantID.UUID(), account).Scan(&psp))
		// The credential is loaded: startup verification for this merchant.
		rt.VerifyProviderPosture(ctx, owned.MerchantID)
		user, err := cp.Core().CreateUser(ctx, uuid.NewString()+"@example.test", "gw"+uuid.NewString()[:8])
		require.NoError(t, err)
		token, _, err := cp.Core().MintAccessToken(ctx, user.ID, nil)
		require.NoError(t, err)
		owner := surface.Client(openrails.WithAPIKey(owned.APIKey), openrails.WithMerchantID(owned.MerchantID))
		_, err = owner.EnsureCustomer(ctx, openrails.CustomerID(uuid.MustParse(user.ID)).String())
		require.NoError(t, err)
		return merchantFixture{owned: owned, key: key, psp: psp, owner: owner, token: token, user: user.ID}
	}
	saveCard := func(m merchantFixture) (int, []byte) {
		var err error
		host, err = orauthkit.NewDelegatedAuthenticator(cp.AuthService().Verifier(), m.owned.MerchantID.String())
		require.NoError(t, err)
		return cutoverHTTPRequest(t, http.MethodPost, surface.BaseURL+"/v1/me/payment-methods", m.token, "", map[string]any{"provider": "nmi", "payment_token": "collectjs-" + uuid.NewString(), "name_on_card": "Gateway Payer"})
	}

	t.Run("armed gateway PSP", func(t *testing.T) {
		m := arm("gwarmed", "gw-armed-key-"+uuid.NewString(), "true")
		reads, _, sandbox := wire.counts(t, m.key)
		require.Equal(t, 1, reads, "startup verifies the credential once on the regular gateway")
		require.Zero(t, sandbox)

		status, raw := saveCard(m)
		require.True(t, status >= 200 && status < 300, "card save: %d %s", status, raw)
		reads, adds, sandbox := wire.counts(t, m.key)
		require.Equal(t, 1, adds, "the vault is created on the gateway with the PSP's own key")
		require.Equal(t, 1, reads, "card save reuses the startup verdict: same merchant, PSP, account and endpoint")
		require.Zero(t, sandbox, "the dedicated sandbox is never contacted")
		var saved struct {
			ID   string `json:"id"`
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(raw, &saved))
		method := saved.ID
		if method == "" {
			method = saved.Data.ID
		}
		require.NotEmpty(t, method)

		// One-time prices declared for this PSP apply without a plan_id.
		prefix := "gw-ppv-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
		manifest := catalog.Manifest{Version: catalog.SupportedVersion}
		for _, key := range []string{prefix + "-token", prefix + "-saved"} {
			manifest.Products = append(manifest.Products, catalog.Product{Key: key, DisplayName: "Gateway pay-per-view", Entitlements: []string{key},
				Prices: []catalog.Price{{Currency: "USD", UnitAmount: 4_990_000, Duration: "indefinite", PSPs: []string{"gwarmed"}}}})
		}
		status, raw = requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/catalog/applications", m.owned.APIKey, catalogApplicationFixture(t, surface.BaseURL, m.owned.APIKey, manifest))
		require.Equal(t, http.StatusOK, status, string(raw))

		customer := openrails.CheckoutCustomerIdentity{ID: m.user, VerifiedEmail: "payer@example.test", Username: "gateway-payer"}
		for i, payment := range []openrails.CheckoutPaymentOptions{
			{PSPID: m.psp.String(), Rail: "nmi", PaymentToken: "collectjs-" + uuid.NewString(), NameOnCard: "Gateway Payer"},
			{PSPID: m.psp.String(), Rail: "nmi", PaymentMethodID: method},
		} {
			var price uuid.UUID
			require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT pr.id FROM billing.prices pr JOIN billing.products p ON p.id=pr.product_id WHERE p.merchant_id=$1 AND p.key=$2`, m.owned.MerchantID.UUID(), manifest.Products[i].Key).Scan(&price))
			options, err := m.owner.ListCheckoutRailOptions(ctx, openrails.PriceID(price).String())
			require.NoError(t, err)
			offered := false
			for _, option := range options {
				offered = offered || option.Rail == "nmi" && option.PSPID == m.psp.String() && option.Mode == "one_off"
			}
			require.True(t, offered, "the one-time price is offered on the armed NMI PSP: %+v", options)

			before := wire.gateway.SaleCount()
			session, err := m.owner.CreateCheckoutSession(ctx, openrails.CreateCheckoutSessionRequest{Customer: customer, PriceID: openrails.PriceID(price).String(), IdempotencyKey: uuid.NewString(), PaymentOptions: payment})
			require.NoError(t, err, "sale %d", i)
			require.Equal(t, "succeeded", session.Status, "sale %d", i)
			require.Equal(t, before+1, wire.gateway.SaleCount(), "sale %d", i)
		}
		wire.mu.Lock()
		require.Equal(t, []string{"secure.networkmerchants.com", "secure.networkmerchants.com"}, wire.saleHosts)
		wire.mu.Unlock()
		reads, _, sandbox = wire.counts(t, m.key)
		require.Equal(t, 1, reads, "sales reuse the same verdict")
		require.Zero(t, sandbox)
	})

	t.Run("live gateway PSP refuses card save", func(t *testing.T) {
		m := arm("gwlive", "gw-live-key-"+uuid.NewString(), "false")
		status, raw := saveCard(m)
		require.True(t, status >= 400 && status < 500, "a disarmed PSP must refuse the card save: %d %s", status, raw)
		reads, adds, sandbox := wire.counts(t, m.key)
		require.Zero(t, adds, "no vault call leaves a disarmed PSP")
		require.Equal(t, 1, reads, "a live verdict holds until the credential is reloaded")
		require.Zero(t, sandbox)
	})
}
