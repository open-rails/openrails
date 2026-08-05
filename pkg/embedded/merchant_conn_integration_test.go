//go:build integration

package embedded

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
	"github.com/open-rails/openrails/pkg/merchant"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// TestEmbedded_RunInMerchantConn covers the seam a host uses to run merchant-owned
// Go work outside any HTTP request or River job, under the RLS-enforcing
// openrails_app role (issue #227). Found via openrails-saas's own staging build
// (#10/#26): internal/platform.Bootstrap calls svc.CreateProduct directly at boot.
//
// or#900 narrowed what this seam is REQUIRED for. Every exported *service.Service
// method now pins its own merchant connection, so a bare facade call with a
// merchant on the context works — the split facade (money surfaces pinned,
// everything else did not) was what made a host's read answer nothing. The seam
// still earns its place for a BLOCK of calls (one connection instead of one per
// call) and for engine-native work that is not a facade method, and its guards
// (unbound engine, mispinned merchant) are unchanged.
func TestEmbedded_RunInMerchantConn(t *testing.T) {
	_, appDSN := dbtest.SharedRLSPostgres(t)
	pool, err := pgxpool.New(context.Background(), appDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	sfx := strings.ToLower(uuid.NewString()[:8])
	cfg := &config.Config{
		// development sidesteps MerchantSourceAPI's non-dev secret-backend
		// requirement (unrelated to this test); the openrails_app role
		// connected below enforces RLS at the Postgres engine level
		// unconditionally, independent of cfg.Env — the #763 app-level gate
		// (development-vs-not) is a different, orthogonal check from the one
		// this test exercises.
		Env:               "development",
		TestMode:          config.CredentialPostureSandbox,
		ProviderWriteMode: config.ProviderWriteModeReadOnly,
		MerchantSource:    config.MerchantSourceAPI,
		SecretBackend:     config.SecretBackendDB,
		DB:                &config.DBConfig{URL: appDSN},
		Auth:              &config.AuthConfig{Issuer: "https://merchant-conn-" + sfx + ".openrails.test"},
	}
	e, err := New(Options{Config: cfg, PGXPool: pool, River: RiverManagedByOpenRails()})
	require.NoError(t, err, "boot as the openrails_app role")
	t.Cleanup(func() { _ = e.Close(context.Background()) })

	ctx := context.Background()
	require.NoError(t, embcp.Attach(ctx, e.App(), cfg, pool))
	res, err := embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{Slug: "conn-" + sfx})
	require.NoError(t, err)

	svc, err := e.Service()
	require.NoError(t, err)

	// or#900: a bare facade call with a merchant on the context now WORKS — the
	// method pins its own connection, so the caller does not have to know which
	// half of the facade it is talking to. Before or#900 this write was rejected
	// by RLS and the matching read answered zero rows and no error.
	selfPinned, err := svc.CreateProduct(merchant.WithID(ctx, res.MerchantID), billingservice.CreateProductRequest{
		Key: "selfpinned-" + sfx, DisplayName: "Self-pinned",
	})
	require.NoError(t, err, "or#900: the facade pins its own merchant connection")
	require.Equal(t, "selfpinned-"+sfx, selfPinned.Key)

	// With NO merchant at all it still fails, and loudly: silence was the bug.
	_, err = svc.CreateProduct(ctx, billingservice.CreateProductRequest{
		Key: "unscoped-" + sfx, DisplayName: "Unscoped",
	})
	require.ErrorContains(t, err, "merchant",
		"an unscoped facade call must name the missing merchant, not answer nothing")

	// The SAME call, wrapped in RunInMerchantConn, succeeds — the seam's value is
	// now one pinned connection for a whole block instead of one per call.
	var created *billingservice.CatalogProduct
	err = e.RunInMerchantConn(ctx, res.MerchantID, func(mctx context.Context) error {
		created, err = svc.CreateProduct(mctx, billingservice.CreateProductRequest{
			Key: "pinned-" + sfx, DisplayName: "Pinned",
		})
		return err
	})
	require.NoError(t, err)
	require.NotNil(t, created)
	require.Equal(t, "pinned-"+sfx, created.Key)

	// A read is scoped the same way: querying by key inside the pinned
	// connection finds it.
	err = e.RunInMerchantConn(ctx, res.MerchantID, func(mctx context.Context) error {
		got, gerr := svc.GetProductByKey(mctx, "pinned-"+sfx)
		require.NoError(t, gerr)
		require.Equal(t, created.ID, got.ID)
		return nil
	})
	require.NoError(t, err)

	// #814 gap 3 — the AUTOMATIC pin. Before the engine is bound to a merchant
	// there is nothing to pin from, and that is a loud error rather than a
	// silent default merchant.
	require.ErrorContains(t,
		e.RunInMerchant(ctx, func(context.Context) error { return nil }),
		"bound to a merchant")

	// Once bound (what an embedding host's provisioning does), the host names
	// no merchant at all — so it cannot name the WRONG one.
	e.App().Runtime.SetConfiguredMerchant(res.MerchantID)
	var autoPinned *billingservice.CatalogProduct
	require.NoError(t, e.RunInMerchant(ctx, func(mctx context.Context) error {
		var perr error
		autoPinned, perr = svc.CreateProduct(mctx, billingservice.CreateProductRequest{
			Key: "autopinned-" + sfx, DisplayName: "Auto-pinned",
		})
		return perr
	}))
	require.Equal(t, "autopinned-"+sfx, autoPinned.Key)

	// And a bound engine refuses a call that names a DIFFERENT merchant — the
	// mispin that would read somebody else's rows.
	require.ErrorContains(t,
		e.RunInMerchantConn(ctx, dbtest.TestMerchantID, func(context.Context) error { return nil }),
		"never reaches another merchant's rows")
}
