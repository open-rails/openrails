//go:build e2e && integration

package ci_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/server/internal/bootstrap/serverboot"
	"github.com/open-rails/openrails/server/internal/operator"
)

// With Vault holding merchant configuration a manifest names merchants only:
// startup refuses one that declares configuration, naming what it declares,
// and provisions one that names the merchant alone, whose configuration staff
// then set in Vault.
func TestManifestBesideVaultNamesMerchantsOnly(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cp := f.newVaultServer(t, nil)
	graph, plane := operator.Of(cp)
	shop := "vaulted-" + uuid.NewString()[:8]
	write := func(body string) string {
		path := filepath.Join(t.TempDir(), "merchants.yaml")
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		return path
	}
	declaring := write(fmt.Sprintf(`version: 1
merchants:
  %s:
    display_name: Vaulted
    psps:
      nmi:
        rail: nmi
        account_id: test
        secrets: {security_key: test}
`, shop))
	err := serverboot.ReconcileBootMerchantManifest(t.Context(), graph.Config, graph, plane, declaring, nil, "")
	require.ErrorContains(t, err, fmt.Sprintf("merchant %q declares display_name, psps", shop))
	_, err = graph.Runtime.Merchants.GetBySlug(t.Context(), shop)
	require.Error(t, err, "a refused manifest provisions nothing")

	naming := write(fmt.Sprintf("version: 1\nmerchants:\n  %s: {}\n", shop))
	require.NoError(t, serverboot.ReconcileBootMerchantManifest(t.Context(), graph.Config, graph, plane, naming, nil, ""))
	m, err := graph.Runtime.Merchants.GetBySlug(t.Context(), shop)
	require.NoError(t, err)
	set, err := graph.Runtime.MerchantConfig.Get(t.Context(), m.ID)
	require.NoError(t, err)
	require.False(t, set.HasMerchant, "Vault holds nothing for it until staff set it")
}
