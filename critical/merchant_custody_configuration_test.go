//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func TestMerchantCustodyConfiguration(t *testing.T) {
	t.Run("OR-J06-S06/merchant-custody-settings-preserve-ownership-and-secrets", func(t *testing.T) {
		a, _ := newPSPWorld(t)
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
	})
}
