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

// TestEmbedded_RunInMerchantConn proves the exact failure this seam exists
// to close: a host calling *service.Service directly (outside any HTTP
// request or River job) MUST pin a merchant-scoped connection, or the
// RLS-enforcing openrails_app role rejects the write outright (issue #227),
// with no distinguishing error from a real permission problem. Found via
// openrails-saas's own staging build (#10/#26): internal/platform.Bootstrap
// calls svc.CreateProduct directly at boot and failed exactly this way under
// the RLS-enforcing role.
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

	// Without a pinned merchant connection, a direct write fails closed under
	// RLS — merchant.WithID alone satisfies merchant.Require but never touches
	// the app.merchant_id session GUC the table's RLS policy checks.
	_, err = svc.CreateProduct(merchant.WithID(ctx, res.MerchantID), billingservice.CreateProductRequest{
		Key: "unpinned-" + sfx, DisplayName: "Unpinned",
	})
	require.Error(t, err, "a merchant-owned write without a pinned connection must be rejected by RLS")

	// The SAME call, wrapped in RunInMerchantConn, succeeds.
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
	// connection finds it; the same query without any pin sees nothing (RLS
	// filters every row, not just writes).
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
