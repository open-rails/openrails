//go:build integration

package integrationharness

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/bootstrap/serverboot"
	"github.com/open-rails/openrails/internal/dbtest"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

// TestStandaloneRouteSurface pins the standalone HTTP route table (#670): the
// neutral net/http stack must serve EXACTLY the surface the retired gin stack
// served. testdata/standalone_route_surface.txt is the golden dumped from the
// last gin build (gin :param syntax converted to ServeMux {param}; the gin
// exact-match "GET /" root is "GET /{$}"). The /auth/* control-plane routes are
// asserted against controlplane.RouteSpecs() dynamically, because that set is
// owned by AuthKit and moves with its version — the assertion here is that
// every spec is mounted, verbatim, under /auth.
func TestStandaloneRouteSurface(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	dbtest.EnsureTestMerchant(ctx, t, h.sharedPool())
	_, appDSN := dbtest.SharedRLSPostgres(t)

	cfg := &config.Config{
		Env:      "dev",
		TestMode: true,
		Host:     "127.0.0.1",
		Port:     0,
		DB:       &config.DBConfig{URL: appDSN},
		Auth:     &config.AuthConfig{Issuer: "https://controlplane.openrails.test"},
	}
	if h.Redis != nil {
		cfg.Redis = &config.RedisConfig{Addr: h.Redis.Options().Addr}
	}

	assembled, err := serverboot.NewServer(cfg, &serverboot.Options{})
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

	// Control-plane surface == the mounted AuthKit RouteSpecs, verbatim.
	cp := embcp.Get(assembled.App)
	require.NotNil(t, cp)
	var wantAuth []string
	for _, spec := range cp.RouteSpecs() {
		wantAuth = append(wantAuth, spec.Method+" /auth"+spec.Path)
	}
	sort.Strings(wantAuth)
	require.Equal(t, wantAuth, gotAuth, "mounted /auth surface must equal controlplane.RouteSpecs()")
	require.NotEmpty(t, wantAuth)
}
