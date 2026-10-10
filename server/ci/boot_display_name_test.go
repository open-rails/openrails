//go:build e2e && integration

package ci_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/server/internal/bootstrap/serverboot"
	"github.com/open-rails/openrails/server/internal/operator"
)

// A merchant the boot manifest creates carries the manifest's display_name:
// the admin API's configuration answers it.
func TestBootManifestDisplayNameIsTheMerchants(t *testing.T) {
	f := newFixture(t)
	slug := uniqueName("boot")
	manifest := filepath.Join(t.TempDir(), "merchants.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte("version: 1\nmerchants:\n  "+slug+":\n    display_name: Boot Shop\n"), 0o600))
	srv := f.newServer(t, nil)
	graph, cp := operator.Of(srv)
	require.NoError(t, serverboot.ReconcileBootMerchantManifest(t.Context(), graph.Config, graph, cp, manifest, nil, ""))

	m, err := graph.Runtime.Merchants.GetBySlug(t.Context(), slug)
	require.NoError(t, err)
	state, err := srv.Client().GetMerchantConfiguration(t.Context(), openrails.ForMerchantID(m.ID))
	require.NoError(t, err)
	require.Equal(t, "Boot Shop", state.DisplayName)
}
