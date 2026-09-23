//go:build integration

package integrationharness

import (
	"context"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	hostconfig "github.com/open-rails/openrails/hostauth/config"
	"github.com/open-rails/openrails/internal/bootstrap/serverboot"
	"github.com/open-rails/openrails/internal/dbtest"
	embcp "github.com/open-rails/openrails/internal/operator"
)

// TestStandaloneRouteSurface pins the standalone HTTP route table (#670): the
// neutral net/http stack must serve exactly the configured central surface.
// testdata/standalone_route_surface.txt records the full configured surface;
// its encrypted secret backend supports provider keys including Solana signers.
// The /auth/* control-plane routes are
// asserted against controlplane.RouteSpecs() dynamically, because that set is
// owned by AuthKit and moves with its version — the assertion here is that
// every spec is mounted under /auth, including the explicit HEAD mirrors that
// AuthKit's mount supplies for GET routes.
func TestStandaloneRouteSurface(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	dbtest.EnsureTestMerchant(ctx, t, h.sharedPool())
	_, appDSN := dbtest.SharedRLSPostgres(t)

	cfg := &config.Config{
		TestMode: config.CredentialPostureSandbox,
		// Pin the complete API-owned provider surface; host-owned mode omits mutations.
		MerchantConfigHTTP: true, AllowCatalogUpdates: true,
		SecretBackend:     config.SecretBackendDB,
		Encryption:        &config.EncryptionConfig{MasterKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="},
		ProviderWriteMode: config.ProviderWriteModeFull,
		Host:              "127.0.0.1",
		Port:              0,
		DB:                &config.DBConfig{URL: appDSN},

		// The golden documents the FULL surface: LLM-backed routes register only
		// when configured, so arm them here (the key is never used).
		LLM: &config.LLMConfig{APIKey: "route-surface-never-used", AskEnabled: true, CatalogCopilotEnabled: true},
	}
	if h.Redis != nil {
		cfg.Redis = &config.RedisConfig{Addr: h.Redis.Options().Addr}
	}

	assembled, err := serverboot.NewServer(context.Background(), cfg, &serverboot.Options{Auth: &hostconfig.AuthConfig{KeysPath: t.TempDir(), Issuer: "https://controlplane.openrails.test", AllowMemory: true, AllowEphemeralSigningKey: true, AllowMissingSenders: true, AllowPrivateNetworkJWKS: true, DirectPeerIP: true}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = assembled.App.Close(context.Background()) })

	var gotBilling, gotAuth []string
	for _, pattern := range assembled.Server.RouteTable() {
		if _, path, ok := strings.Cut(pattern, " "); ok && (path == "/auth" || strings.HasPrefix(path, "/auth/")) {
			gotAuth = append(gotAuth, pattern)
			continue
		}
		gotBilling = append(gotBilling, pattern)
	}
	sort.Strings(gotBilling)
	sort.Strings(gotAuth)

	// UPDATE_ROUTE_GOLDEN=1 rewrites the golden from the live route table (for
	// deliberate surface changes); the assertions below then pin the new state.
	if os.Getenv("UPDATE_ROUTE_GOLDEN") != "" {
		require.NoError(t, os.WriteFile("testdata/standalone_route_surface.txt",
			[]byte(strings.Join(gotBilling, "\n")+"\n"), 0o644))
		t.Log("regenerated testdata/standalone_route_surface.txt")
	}

	// Billing surface == golden.
	raw, err := os.ReadFile("testdata/standalone_route_surface.txt")
	require.NoError(t, err)
	want := strings.Split(strings.TrimSpace(string(raw)), "\n")
	sort.Strings(want)
	require.Equal(t, want, gotBilling, "standalone billing route surface drifted from the #670 golden")

	// Control-plane surface == the AuthKit specs plus their HEAD mirrors.
	cp := embcp.Get(assembled.App)
	require.NotNil(t, cp)
	var wantAuth []string
	explicitHeads := map[string]bool{}
	for _, spec := range cp.RouteSpecs() {
		if spec.Method == http.MethodHead {
			explicitHeads[spec.Path] = true
		}
	}
	for _, spec := range cp.RouteSpecs() {
		wantAuth = append(wantAuth, spec.Method+" /auth"+spec.Path)
		if spec.Method == http.MethodGet && !explicitHeads[spec.Path] {
			wantAuth = append(wantAuth, http.MethodHead+" /auth"+spec.Path)
		}
	}
	sort.Strings(wantAuth)
	require.Equal(t, wantAuth, gotAuth, "mounted /auth surface must equal controlplane.RouteSpecs() with HEAD mirrors")
	require.NotEmpty(t, wantAuth)
}
