//go:build e2e && integration

package ci_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/server/internal/bootstrap/serverboot"
	"github.com/open-rails/openrails/server/internal/operator"
)

// A boot manifest that still gives a merchant an api_host is refused, naming
// the key and saying it was removed, and provisions nothing.
func TestManifestRefusesAPIHost(t *testing.T) {
	f := newFixture(t)
	srv := f.newServer(t, nil)
	graph, cp := operator.Of(srv)
	slug := uniqueName("hosted")
	manifest := filepath.Join(t.TempDir(), "merchants.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte("version: 1\nmerchants:\n  "+slug+":\n    display_name: Hosted\n    api_host: "+slug+".api.billing.example\n"), 0o600))

	err := serverboot.ReconcileBootMerchantManifest(t.Context(), graph.Config, graph, cp, manifest, nil, "")
	require.ErrorContains(t, err, "merchants."+slug+".api_host was removed")
	_, err = graph.Runtime.Merchants.GetBySlug(t.Context(), slug)
	require.Error(t, err, "a refused manifest provisions nothing")
}
