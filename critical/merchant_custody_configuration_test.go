//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/stretchr/testify/require"
)

func TestMerchantCustodyConfiguration(t *testing.T) {
	t.Run("OR-J06-S06/merchant-custody-settings-preserve-ownership-and-secrets", func(t *testing.T) {
		var redirected atomic.Int32
		foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1); w.WriteHeader(http.StatusOK) }))
		t.Cleanup(foreign.Close)
		vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("BT-API-KEY")
			if key == "private-redirect" {
				http.Redirect(w, r, foreign.URL, http.StatusTemporaryRedirect)
				return
			}
			if r.Method != http.MethodGet || r.URL.Path != "/applications/key" || (key != "private-owned" && key != "private-rotated" && key != "private-foreign") {
				http.Error(w, "credential rejected "+key, http.StatusUnauthorized)
				return
			}
			tenant := "00000000-0000-4000-8000-000000000011"
			if key == "private-foreign" {
				tenant = "00000000-0000-4000-8000-000000000012"
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"tenant_id": tenant})
		}))
		t.Cleanup(vendor.Close)
		target, err := url.Parse(vendor.URL)
		require.NoError(t, err)
		wire := http.DefaultTransport.(*http.Transport).Clone()
		t.Cleanup(wire.CloseIdleConnections)
		gateway := nmimock.New(nmimock.Options{})
		t.Cleanup(gateway.Close)
		gatewayTarget, err := url.Parse(gateway.URL())
		require.NoError(t, err)
		a, _ := newPSPWorld(t, func(w *world) {
			w.deps.NMITransport = loopbackWire{target: gatewayTarget, wire: wire}
			w.deps.CustodianCredentialTransport = custodianProbeWire{loopback: loopbackWire{target: target, wire: wire}, wire: wire}
		})
		for name, client := range map[string]*openrails.Client{"alpha": a.alphaRemote, "beta": a.betaRemote} {
			_, err := a.deps.Vault.Logical().WriteWithContext(t.Context(), a.cfg.Vault.KVMount+"/data/openrails/merchants/"+client.MerchantID().UUID().String()+"/custodians/cards", map[string]any{"data": map[string]any{
				"kind": "basis_theory", "environment": "test", "account_id": "tenant-" + name,
				"public_api_key": "public-" + name, "secrets": map[string]string{"api_key": "private-" + name},
			}})
			require.NoError(t, err)
			_, err = client.UpdateMerchantConfiguration(t.Context(), billing.UpdateMerchantConfigurationParams{DisplayName: ptr(name)})
			require.NoError(t, err)
		}
		status, raw := a.request(t, http.MethodGet, "/v1/merchant/custodians", a.alphaKey.Secret, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var page struct {
			Data []struct {
				Key, Kind   string
				AccountID   string `json:"account_id"`
				Credentials map[string]struct{ Configured bool }
			}
		}
		require.NoError(t, json.Unmarshal(raw, &page))
		require.Len(t, page.Data, 1)
		require.Equal(t, "cards", page.Data[0].Key)
		require.Equal(t, "tenant-alpha", page.Data[0].AccountID)
		require.True(t, page.Data[0].Credentials["api_key"].Configured)
		require.NotContains(t, string(raw), "private-alpha")
		require.NotContains(t, string(raw), "private-beta")
		status, raw = a.request(t, http.MethodGet, "/v1/merchant/custodians", a.betaKey.Secret, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.NoError(t, json.Unmarshal(raw, &page))
		require.Len(t, page.Data, 1)
		require.Equal(t, "tenant-beta", page.Data[0].AccountID)
		status, raw = a.request(t, http.MethodGet, "/v1/merchant/custodians", a.bobToken, nil)
		require.Equal(t, http.StatusForbidden, status, string(raw))
		status, raw = a.request(t, http.MethodPut, "/v1/merchant/custodians/owned", a.alphaKey.Secret, map[string]any{
			"expected_revision": 0, "kind": "basis_theory", "environment": "test",
			"account_id":  "00000000-0000-4000-8000-000000000011",
			"settings":    map[string]any{"public_api_key": "public-owned"},
			"credentials": map[string]string{"api_key": "private-owned"},
		})
		require.Equal(t, http.StatusOK, status, string(raw))
		var saved billing.Custodian
		require.NoError(t, json.Unmarshal(raw, &saved))
		require.Equal(t, "owned", saved.Key)
		require.EqualValues(t, 1, saved.Revision)
		require.True(t, saved.Credentials["api_key"].Configured)
		require.NotContains(t, string(raw), "private-owned")
		request := billing.PutCustodianParams{ExpectedRevision: 1, Kind: "basis_theory", Environment: "test", AccountID: "00000000-0000-4000-8000-000000000011"}
		before, err := a.alphaRemote.ListCustodians(t.Context())
		require.NoError(t, err)
		require.Len(t, before.Items, 2)
		for _, check := range []struct {
			name, key string
			merchant  *openrails.Client
			change    func(*billing.PutCustodianParams)
			status    int
		}{
			{"stale", "owned", a.alphaRemote, func(r *billing.PutCustodianParams) {
				r.ExpectedRevision = 0
				r.Settings = map[string]any{"public_api_key": "stale-public"}
			}, 409},
			{"identity change", "owned", a.alphaRemote, func(r *billing.PutCustodianParams) {
				r.AccountID = "00000000-0000-4000-8000-000000000012"
				r.Credentials = map[string]string{"api_key": "private-foreign"}
			}, 409},
			{"wrong account proof", "wrong-account", a.alphaRemote, func(r *billing.PutCustodianParams) {
				r.ExpectedRevision = 0
				r.AccountID = "00000000-0000-4000-8000-000000000012"
				r.Settings = map[string]any{"public_api_key": "public-owned"}
				r.Credentials = map[string]string{"api_key": "private-owned"}
			}, 400},
			{"other merchant", "owned", a.betaRemote, func(r *billing.PutCustodianParams) {
				r.ExpectedRevision = 0
				r.Settings = map[string]any{"public_api_key": "public-owned"}
				r.Credentials = map[string]string{"api_key": "private-owned"}
			}, 409},
			{"private key in public settings", "owned", a.alphaRemote, func(r *billing.PutCustodianParams) { r.Settings = map[string]any{"public_api_key": "private-owned"} }, 400},
			{"rejected credential", "owned", a.alphaRemote, func(r *billing.PutCustodianParams) { r.Credentials = map[string]string{"api_key": "private-rejected"} }, 400},
			{"credential redirect", "owned", a.alphaRemote, func(r *billing.PutCustodianParams) { r.Credentials = map[string]string{"api_key": "private-redirect"} }, 503},
		} {
			t.Run(check.name, func(t *testing.T) {
				in := request
				check.change(&in)
				_, err := check.merchant.PutCustodian(t.Context(), check.key, in)
				var refusal *billing.StatusError
				require.ErrorAs(t, err, &refusal)
				require.Equal(t, check.status, refusal.Status)
				for _, secret := range []string{"private-owned", "private-rejected", "private-redirect", "private-foreign"} {
					require.NotContains(t, err.Error(), secret)
				}
			})
		}
		require.Zero(t, redirected.Load(), "provider redirects cannot forward a private key to another endpoint")
		after, err := a.alphaRemote.ListCustodians(t.Context())
		require.NoError(t, err)
		require.Equal(t, before, after)
		for _, check := range []struct {
			merchant billing.MerchantID
			key      string
		}{{a.beta.MerchantID(), "owned"}, {a.alpha.MerchantID(), "wrong-account"}} {
			unwritten, err := a.deps.Vault.Logical().ReadWithContext(t.Context(), a.cfg.Vault.KVMount+"/data/openrails/merchants/"+check.merchant.UUID().String()+"/custodians/"+check.key)
			require.NoError(t, err)
			require.Nil(t, unwritten, "refused account claims do not write credential documents")
		}
		request.Credentials = map[string]string{"api_key": "private-rotated"}
		rotated, err := a.alphaRemote.PutCustodian(t.Context(), "owned", request)
		require.NoError(t, err)
		require.Equal(t, saved.AccountID, rotated.AccountID)
		require.EqualValues(t, 2, rotated.Revision)
		require.Equal(t, saved.Settings, rotated.Settings)
		raw, err = json.Marshal(rotated)
		require.NoError(t, err)
		require.NotContains(t, string(raw), "private-rotated")
		status, raw = a.request(t, http.MethodPost, "/v1/merchant/psps", a.alphaKey.Secret, map[string]any{
			"key": "nmi", "rail": "nmi", "environment": "test", "gateway_id": "gateway-config-alpha",
			"custodian": "owned", "tokenization_key": "collect-alpha",
			"credentials": map[string]string{"security_key": "critical-nmi-key", "webhook_signing_secret": "critical-nmi-webhook"},
		})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var processor billing.PSP
		require.NoError(t, json.Unmarshal(raw, &processor))
		require.Equal(t, "owned", processor.Custodian)
	})
}

type custodianProbeWire struct {
	loopback loopbackWire
	wire     http.RoundTripper
}

func (w custodianProbeWire) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host == "api.basistheory.com" || r.URL.Host == "api.test.basistheory.com" {
		return w.loopback.RoundTrip(r)
	}
	return w.wire.RoundTrip(r)
}
