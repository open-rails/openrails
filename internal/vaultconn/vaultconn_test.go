package vaultconn

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	vaultapi "github.com/hashicorp/vault/api"
	"github.com/open-rails/openrails/internal/config"
	"github.com/stretchr/testify/require"
)

func TestMalformedPlatformMountRefusedWithoutEngineBoot(t *testing.T) {
	for _, mount := range []string{"cards/../other", "/", "card secrets"} {
		t.Run(mount, func(t *testing.T) {
			for _, borrowed := range []bool{false, true} {
				cfg := &config.Config{
					Vault:     &config.VaultConfig{Address: "http://127.0.0.1:1", Token: "fixture", KVMount: "merchant"},
					CardVault: &config.CardVaultConfig{Kind: "basis_theory", TenantID: "tenant", KVMount: mount},
				}
				var client *vaultapi.Client
				if borrowed {
					var err error
					client, err = vaultapi.NewClient(&vaultapi.Config{Address: cfg.Vault.Address})
					require.NoError(t, err)
				}
				conn, err := Open(t.Context(), cfg, client)
				if conn != nil {
					t.Cleanup(conn.Close)
				}
				require.ErrorContains(t, err, "card_vault.kv_mount")
				require.Nil(t, conn)
			}
		})
	}
}

func TestPlatformCredentialReadMount(t *testing.T) {
	for _, tc := range []struct {
		name, merchant, platform, read string
		missing                        bool
	}{
		{name: "files", platform: "cards", read: "cards"},
		{name: "shared", merchant: "merchant", read: "merchant"},
		{name: "separate", merchant: "merchant", platform: "cards", read: "cards"},
		{name: "no fallback after missing document", merchant: "merchant", platform: "cards", read: "cards", missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var reads []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				reads = append(reads, r.URL.Path)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if tc.missing && r.URL.Path == "/v1/cards/data/platform_vault/tenant" {
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"errors":[]}`))
					return
				}
				_, _ = w.Write([]byte(`{"data":{"data":{"api_key":"fixture"},"metadata":{"version":1}}}`))
			}))
			t.Cleanup(server.Close)
			client, err := vaultapi.NewClient(&vaultapi.Config{Address: server.URL})
			require.NoError(t, err)
			cfg := &config.Config{Vault: &config.VaultConfig{KVMount: tc.merchant},
				CardVault: &config.CardVaultConfig{Kind: "basis_theory", TenantID: "tenant", KVMount: tc.platform}}
			conn, err := Open(t.Context(), cfg, client)
			require.NoError(t, err)
			t.Cleanup(conn.Close)
			require.Equal(t, tc.merchant != "", conn.KV != nil)
			doc, err := conn.PlatformKV.ReadDocument(t.Context(), "platform_vault/tenant")
			require.NoError(t, err)
			require.Equal(t, !tc.missing, doc.Found)
			mu.Lock()
			require.Equal(t, []string{"/v1/" + tc.read + "/data/platform_vault/tenant"}, reads)
			mu.Unlock()
		})
	}
}
