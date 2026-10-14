//go:build e2e && integration

package subscriptions_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	vaultapi "github.com/hashicorp/vault/api"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/vaulttest"
	"github.com/stretchr/testify/require"
)

func TestPlatformVaultWithFileConfiguration(t *testing.T) {
	for _, borrowed := range []bool{false, true} {
		name := "supervised login"
		if borrowed {
			name = "borrowed client"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w, bt := preparePlatformSubscriptionWorld(t)
			v := vaulttest.New(t)
			const prefix = "card-credentials"
			document := prefix + "/platform_vault/" + platformTenant
			policy := fmt.Sprintf(`path %q { capabilities = ["read"] }`, v.Mount+"/data/"+document)
			require.NoError(t, v.Client().Sys().PutPolicy(v.Mount, policy))
			t.Cleanup(func() { _ = v.Client().Sys().DeletePolicy(v.Mount) })
			secret, err := v.Client().Auth().Token().Create(&vaultapi.TokenCreateRequest{Policies: []string{v.Mount}})
			require.NoError(t, err)
			t.Cleanup(func() { _ = v.Client().Auth().Token().RevokeAccessor(secret.Auth.Accessor) })

			upstream, err := url.Parse(v.Addr)
			require.NoError(t, err)
			proxy := httputil.NewSingleHostReverseProxy(upstream)
			var refusal, credentialReads, merchantReads atomic.Int64
			refusal.Store(http.StatusServiceUnavailable)
			server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/merchants/") || strings.HasSuffix(r.URL.Path, "/credential_fingerprint_key") {
					merchantReads.Add(1)
				}
				if r.URL.Path == "/v1/"+v.Mount+"/data/"+document {
					credentialReads.Add(1)
					if status := refusal.Load(); status != 0 {
						rw.Header().Set("Content-Type", "application/json")
						rw.WriteHeader(int(status))
						_, _ = rw.Write([]byte(`{"errors":["credential document unavailable"]}`))
						return
					}
				}
				proxy.ServeHTTP(rw, r)
			}))
			t.Cleanup(server.Close)
			t.Cleanup(w.stop)
			configure := w.cfg
			w.cfg = func(cfg *config.Config) {
				configure(cfg)
				cfg.Vault = &config.VaultConfig{Address: server.URL, Token: secret.Auth.ClientToken, ScopePrefix: prefix}
				cfg.PlatformVault.KVMount = v.Mount
			}
			if borrowed {
				client, err := vaultapi.NewClient(&vaultapi.Config{Address: server.URL})
				require.NoError(t, err)
				client.SetToken(secret.Auth.ClientToken)
				w.deps = func(deps *openrails.Deps) { deps.Vault = client }
			}
			w.settings = billing.MerchantSettings{CardVault: billing.CardVaultPlatform, ChargePSPs: []string{"nmi"}}
			w.start()
			rt := engine.Graph(w.rt).Runtime
			require.False(t, rt.MerchantConfigManageable())
			_, err = w.client[remote].UpdateMerchantConfiguration(t.Context(), setVault("nmi"))
			require.Error(t, err, "card credentials must not enable configuration edits")
			settings, err := w.client[remote].GetMerchantConfiguration(t.Context())
			require.NoError(t, err)
			require.Equal(t, billing.CardVaultPlatform, settings.MerchantSettings.CardVault)
			mid := w.client[remote].MerchantID().UUID().String()
			v.Put(t, document, map[string]any{
				"api_key": "platform-private-test", "collection_applications": map[string]any{
					mid: map[string]any{"id": bt.application, "public_api_key": "platform-public-test"},
				},
			})
			c := w.newCustomer()
			const number = "4111111111111111"
			req := map[string]any{"psp_id": w.psp["nmi"], "token": bt.collect(number)}
			status, _ := c.call(http.MethodPost, "/payment-methods", "", req)
			require.GreaterOrEqual(t, status, 400)
			bt.mu.Lock()
			require.Empty(t, bt.verifications)
			bt.mu.Unlock()
			refusal.Store(0)
			saved := unwrap(c.must(http.MethodPost, "/payment-methods", "", req))
			method := saved["id"].(string)
			methodID, err := billing.ParsePaymentMethodID(method)
			require.NoError(t, err)
			paid := c.mustPurchase(remote, order{price: w.membership("platform:file-config", 9_990_000).ID, rail: "nmi", method: method})
			subID := *paid.SubscriptionID
			end := *w.subscription(remote, subID).CurrentPeriodEndsAt
			w.advanceHealthyTo(end)
			refusal.Store(http.StatusForbidden)
			w.runRenewals()
			require.Len(t, bt.salesOf(number), 1, "unreadable credentials cannot charge a renewal")
			require.Equal(t, end, *w.subscription(remote, subID).CurrentPeriodEndsAt)
			_, err = w.client[remote].GetMerchantConfiguration(t.Context())
			require.NoError(t, err, "file settings survive a credential read failure")
			require.True(t, c.entitled("platform:file-config"))
			refusal.Store(0)
			w.runRenewals()
			w.until(func() bool { return w.subscription(remote, subID).CurrentPeriodEndsAt.After(end) }, "renewal recovers through the existing worker")
			w.runRenewals()
			require.Len(t, bt.salesOf(number), 2, "replay does not take a second renewal payment")
			c.must(http.MethodPost, "/subscriptions/"+subID.String()+"/cancel", "", map[string]any{"reason": "finished"})
			status, _ = c.call(http.MethodDelete, "/payment-methods/"+method, "", nil)
			require.Contains(t, []int{http.StatusAccepted, http.StatusNoContent}, status)
			w.until(func() bool {
				var removed bool
				require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT status='removed' FROM billing.payment_methods WHERE id=$1`), methodID.UUID()).Scan(&removed))
				return removed
			}, "platform deletion uses the independent credential mount")
			require.Positive(t, credentialReads.Load())
			require.Zero(t, merchantReads.Load(), "neither merchant documents nor the fingerprint key belong in this Vault store")
			// Stop before revoking the fixture's token or removing its mount.
			w.stop()
		})
	}
}
