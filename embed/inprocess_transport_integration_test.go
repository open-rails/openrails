//go:build integration

package embed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/embedded"
	"github.com/open-rails/openrails/pkg/merchant"
)

// TestInProcessTransportAuthTraversal proves the #685 auth contract on the REAL
// middleware chain (no stubs): the in-process merchant handler rejects any
// request that does not carry the transport's context-attached host principal —
// bearer/headers cannot forge it, since headers can arrive from the network and
// the gate never consults them for this credential — while the transport-
// injected principal authorizes, gated on permissions like every credential.
func TestInProcessTransportAuthTraversal(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	dbtest.EnsureTestMerchant(ctx, t, pool)

	cfg := &config.Config{Env: "dev", DB: &config.DBConfig{URL: dsn}}
	rt, err := New(ctx, Options{Options: embedded.Options{Config: cfg}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	rt.emb.App().Runtime.ConfiguredMerchant = dbtest.TestMerchantID

	handler := newServiceHandler(rt.emb.App().Runtime)

	// 1) Network-shaped request: NO context principal. The real gate rejects it
	// 401 even with a bearer and a spoofed header — nothing wire-supplied can
	// impersonate the host.
	req := httptest.NewRequest(http.MethodGet, "/v1/merchant/settings", nil)
	req.Header.Set("Authorization", "Bearer in-process-host")
	req.Header.Set("X-OpenRails-Host-Principal", dbtest.TestMerchantID.String())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code,
		"request without the transport's context marker must be rejected by the real middleware")

	// 2) The SAME request with the transport's context-attached principal passes.
	allowCtx := billingauth.WithHostPrincipal(
		merchant.WithID(ctx, dbtest.TestMerchantID),
		&billingauth.HostPrincipal{MerchantID: dbtest.TestMerchantID, Permissions: hostPermissions()},
	)
	req2 := httptest.NewRequest(http.MethodGet, "/v1/merchant/settings", nil).WithContext(allowCtx)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	require.Equal(t, http.StatusOK, rec2.Code, "context-attached host principal must authorize: %s", rec2.Body.String())

	// 3) The principal is still permission-gated: no permissions -> 403.
	deniedCtx := billingauth.WithHostPrincipal(
		merchant.WithID(ctx, dbtest.TestMerchantID),
		&billingauth.HostPrincipal{MerchantID: dbtest.TestMerchantID},
	)
	req3 := httptest.NewRequest(http.MethodGet, "/v1/merchant/settings", nil).WithContext(deniedCtx)
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, req3)
	require.Equal(t, http.StatusForbidden, rec3.Code, "host principal without permissions must be denied")

	// 4) End-to-end through the unified client: the transport injects the
	// principal, the real gate authorizes, the handler answers.
	c := rt.Client()
	_, err = c.GetMerchantSettings(ctx)
	require.NoError(t, err, "unified client through in-process transport")
	require.NoError(t, openrails.Verify(ctx, c), "Verify through in-process transport")
}
