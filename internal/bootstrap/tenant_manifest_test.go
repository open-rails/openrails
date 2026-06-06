package bootstrap

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/pkg/tenant"
	"github.com/stretchr/testify/require"
)

func TestLoadTenantManifestRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tenants.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
version: 2
tenants:
  - slug: cozy-art
    operator_tenant_slug: legacy
`), 0o600))

	_, err := loadTenantManifest(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "operator_tenant_slug")
}

func TestLoadTenantManifestRequiresVersion2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tenants.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
version: 1
tenants:
  - slug: cozy-art
`), 0o600))

	_, err := loadTenantManifest(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "version must be 2")
}

func TestResolveManifestResourcesMapsTenantAliasAndLeavesHostResourcesOpaque(t *testing.T) {
	tid := tenant.DefaultID
	resources, err := resolveManifestResources([]ManifestResource{
		{Kind: controlplane.ResourceKindTenant, ID: "$tenant"},
		{Kind: "custom.resource", ID: "alpha"},
	}, tid, "cozy-art")
	require.NoError(t, err)
	require.Len(t, resources, 2)
	require.Equal(t, controlplane.ResourceKindTenant, resources[0].Kind)
	require.Equal(t, tid.String(), resources[0].ID)
	require.Equal(t, "custom.resource", resources[1].Kind)
	require.Equal(t, "alpha", resources[1].ID)
}

func TestResolveManifestResourcesRequiresResources(t *testing.T) {
	_, err := resolveManifestResources(nil, tenant.DefaultID, "cozy-art")
	require.Error(t, err)
	require.Contains(t, err.Error(), "resources are required")
}

func TestVaultServiceTokenOutputPreservesExistingFields(t *testing.T) {
	data := map[string]any{"token": "preserved-token", "other": "keep"}
	var writeBody map[string]map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "root", r.Header.Get("X-Vault-Token"))
		require.Equal(t, "/v1/secret/data/openrails/runtime", r.URL.Path)
		switch r.Method {
		case http.MethodGet:
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": data}}))
		case http.MethodPut:
			require.NoError(t, json.NewDecoder(r.Body).Decode(&writeBody))
			data = writeBody["data"]
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{Vault: &config.VaultConfig{Address: srv.URL, Token: "root", KVMount: "secret"}}
	outputs := []ManifestOutput{{Vault: &ManifestVaultOutput{Path: "openrails/runtime", Field: "token"}}}

	existing, err := readExistingServiceTokenOutput(t.Context(), cfg, outputs)
	require.NoError(t, err)
	require.Equal(t, "preserved-token", existing)

	require.NoError(t, writeServiceTokenOutputs(t.Context(), cfg, outputs, existing))
	require.Equal(t, "preserved-token", writeBody["data"]["token"])
	require.Equal(t, "keep", writeBody["data"]["other"])
}
