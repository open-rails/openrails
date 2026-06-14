//go:build integration

package repo

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

// This test proves end-to-end #227 RLS enforcement on a REAL openrails.* table
// through a REAL repo: it runs the actual migrations (so 001_schema.up.sql applies
// its policies + creates openrails_app), connects as openrails_app, and drives
// ProductRepo.GetAll — a no-filter read — to show it returns ONLY the pinned
// tenant's rows. This is the request-path chain (middleware pins conn -> repo
// uses db.Q(ctx) -> Postgres RLS) exercised on production code.
//
// Requires OPENRAILS_TEST_DB_DSN (a SUPER/admin DSN). Run against a --network host
// Postgres when testcontainers is flaky:
//
//	docker run -d --network host postgres:18-alpine -c port=5599 -c listen_addresses=127.0.0.1
//	OPENRAILS_TEST_DB_DSN=postgresql://test:test@127.0.0.1:5599/openrails?sslmode=disable \
//	  go test -tags integration -run TestRLSRealTable ./internal/db/repo/

func TestRLSRealTable_ProductRepo_Under_OpenRailsApp(t *testing.T) {
	ctx := context.Background()

	// Shared, fully-migrated DB (incl. 050: RLS + FORCE + openrails_app) with the
	// app role's login already enabled. super bypasses RLS; app enforces it.
	superDSN, appDSN := dbtest.SharedRLSPostgres(t)

	// Idempotently seed two tenants' products as super (super bypasses RLS, so it
	// can write any tenant's rows). Slugs carry a unique suffix so this test's
	// fixtures never collide with sibling tests sharing the DB on the global
	// products.slug / tenants.slug UNIQUE constraints.
	suffix := uuid.NewString()[:8]
	tenantA := uuid.NewString()
	tenantB := uuid.NewString()
	productA := uuid.NewString()
	productB := uuid.NewString()
	slugA := "prod-a-" + suffix
	slugB := "prod-b-" + suffix
	super, err := db.NewDB(&config.DBConfig{URL: superDSN})
	require.NoError(t, err)
	defer super.Close()
	for _, stmt := range []string{
		`INSERT INTO openrails.merchants (id, slug, name) VALUES
		   ('` + tenantA + `','tenant-` + suffix + `-a','A'), ('` + tenantB + `','tenant-` + suffix + `-b','B')
		 ON CONFLICT (id) DO NOTHING`,
		`INSERT INTO openrails.products (id, merchant_id, slug, display_name) VALUES
		   ('` + productA + `','` + tenantA + `','` + slugA + `','Product A'),
		   ('` + productB + `','` + tenantB + `','` + slugB + `','Product B')
		 ON CONFLICT (id) DO NOTHING`,
	} {
		_, e := super.Pool().Exec(ctx, stmt)
		require.NoError(t, e, stmt)
	}

	// Connect as the unprivileged openrails_app role (RLS ENFORCES).
	app, err := db.NewDB(&config.DBConfig{URL: appDSN})
	require.NoError(t, err)
	defer app.Close()
	posture, err := app.CheckRLSPosture(ctx)
	require.NoError(t, err)
	require.True(t, posture.Enforcing, "must connect as an RLS-enforcing role")

	repo := NewProductRepo(app)

	// (1) Without a pinned tenant connection: fail-closed (GetAll sees nothing).
	bare, err := repo.GetAll(ctx)
	require.NoError(t, err)
	require.Len(t, bare, 0, "no pinned tenant conn => RLS fail-closed, repo sees nothing")

	// (2) Pinned to tenant A: the real repo's no-filter GetAll returns ONLY
	// tenant A's product — Postgres enforced it, not a WHERE clause in the repo.
	ctxA := merchant.WithID(ctx, mustTID(tenantA))
	connA, releaseA, err := app.WithTenantConn(ctxA)
	require.NoError(t, err)
	gotA, err := repo.GetAll(connA)
	require.NoError(t, err)
	require.Len(t, gotA, 1, "tenant A sees exactly its own product")
	require.Equal(t, slugA, gotA[0].Slug)
	releaseA()

	// (3) Pinned to tenant B: sees only tenant B's product. No cross-tenant bleed.
	ctxB := merchant.WithID(ctx, mustTID(tenantB))
	connB, releaseB, err := app.WithTenantConn(ctxB)
	require.NoError(t, err)
	gotB, err := repo.GetAll(connB)
	require.NoError(t, err)
	require.Len(t, gotB, 1)
	require.Equal(t, slugB, gotB[0].Slug)
	releaseB()
}

func mustTID(s string) merchant.ID {
	id, err := merchant.ParseID(s)
	if err != nil {
		panic(err)
	}
	return id
}
