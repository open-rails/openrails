//go:build integration

package catalog

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// This test exercises the production product repository with two merchants.
// Its explicit predicates must isolate both normal and owner connections, and
// missing merchant authority must fail before a query can expose any rows.
//
// Requires OPENRAILS_TEST_DB_DSN (a SUPER/admin DSN). Run against a --network host
// Postgres when testcontainers is flaky:
//
//	docker run -d --network host postgres:18-alpine -c port=5599 -c listen_addresses=127.0.0.1
//	OPENRAILS_TEST_DB_DSN=postgresql://test:test@127.0.0.1:5599/openrails?sslmode=disable \
//	  go test -tags integration -run TestRLSRealTable ./internal/modules/catalog/

func TestProductRepoMerchantIsolation(t *testing.T) {
	ctx := context.Background()

	// Shared, fully migrated test storage with privileged and normal logins.
	superDSN, appDSN := dbtest.SharedRLSPostgres(t)

	// Idempotently seed two tenants' products as super (super bypasses RLS, so it
	// can write any merchant's rows). Keys carry a unique suffix so this test's
	// fixtures never collide with sibling tests sharing the DB.
	suffix := uuid.NewString()[:8]
	tenantA := uuid.NewString()
	tenantB := uuid.NewString()
	productA := uuid.NewString()
	productB := uuid.NewString()
	keyA := "prod-a-" + suffix
	keyB := "prod-b-" + suffix
	super, err := db.NewDB(t.Context(), &config.DBConfig{URL: superDSN})
	require.NoError(t, err)
	defer super.Close()
	for _, stmt := range []string{
		`INSERT INTO billing.merchants (id, slug) VALUES
		   ('` + tenantA + `','merchant-` + suffix + `-a'), ('` + tenantB + `','merchant-` + suffix + `-b')
		 ON CONFLICT (id) DO NOTHING`,
		`INSERT INTO billing.products (id, merchant_id, key, display_name) VALUES
		   ('` + productA + `','` + tenantA + `','` + keyA + `','Product A'),
		   ('` + productB + `','` + tenantB + `','` + keyB + `','Product B')
		 ON CONFLICT (id) DO NOTHING`,
	} {
		_, e := super.Pool().Exec(ctx, stmt)
		require.NoError(t, e, stmt)
	}

	// Connect as the normal runtime login.
	app, err := db.NewDB(t.Context(), &config.DBConfig{URL: appDSN})
	require.NoError(t, err)
	defer app.Close()
	for _, database := range []*db.DB{app, super} {
		repo := NewProductService(database)

		// (1) Missing authority fails closed, independently of the database role.
		bare, err := repo.GetAll(ctx)
		require.Error(t, err)
		require.Empty(t, bare)

		// (2) Selected merchant A sees only its row through explicit predicates.
		ctxA := merchant.WithID(ctx, mustTID(tenantA))
		connA, releaseA, err := database.WithMerchantConn(ctxA)
		require.NoError(t, err)
		gotA, err := repo.GetAll(connA)
		require.NoError(t, err)
		require.Len(t, gotA, 1, "merchant A sees exactly its own product")
		require.Equal(t, keyA, gotA[0].Key)
		releaseA()

		// (3) Pinned to merchant B: sees only merchant B's product. No cross-merchant bleed.
		ctxB := merchant.WithID(ctx, mustTID(tenantB))
		connB, releaseB, err := database.WithMerchantConn(ctxB)
		require.NoError(t, err)
		gotB, err := repo.GetAll(connB)
		require.NoError(t, err)
		require.Len(t, gotB, 1)
		require.Equal(t, keyB, gotB[0].Key)
		releaseB()
	}
}

func mustTID(s string) merchant.ID {
	id, err := merchant.ParseID(s)
	if err != nil {
		panic(err)
	}
	return id
}
