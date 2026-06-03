//go:build integration

package productaccess

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	postgresmigrations "github.com/open-rails/openrails/migrations/postgres"
	"github.com/open-rails/openrails/pkg/tenant"
)

// This suite proves the migration-063 product_access_grants table + its
// migration-050-form RLS policy enforce durable ownership AND tenant isolation
// when the app connects as the unprivileged openrails_app role (issue #227/#250).
// It runs the REAL 063 up SQL (read from the embedded migrations FS), then drives
// the productaccess.Service through db.RunInTenantTx (which pins app.tenant_id),
// so it validates the real schema, the real policy, and the GUC plumbing.
//
// Asserts: purchase -> grant created; duplicate purchase -> still one grant;
// refund -> grant revoked; tenant isolation (tenant A's grant invisible to B).

var (
	pagTenantA = mustTenant("00000000-0000-0000-0000-0000000000a1")
	pagTenantB = mustTenant("00000000-0000-0000-0000-0000000000b2")
)

func mustTenant(s string) tenant.ID {
	id, err := tenant.ParseID(s)
	if err != nil {
		panic(err)
	}
	return id
}

// prereqDDL creates the minimal environment migration-063 assumes: the billing
// schema, a uuidv7() function (Postgres < 18 has none), the products table the
// grant FK references, and the unprivileged openrails_app role (WITH LOGIN for
// the test) that makes RLS actually enforce.
const pagPrereqDDL = `
CREATE SCHEMA IF NOT EXISTS billing;

-- uuidv7() shim for engines without the native function. Migration 063 uses it
-- as the id default. (On Postgres 18 the native one is used instead; CREATE OR
-- REPLACE keeps this harmless.)
CREATE OR REPLACE FUNCTION uuidv7() RETURNS uuid AS $$
    SELECT gen_random_uuid();
$$ LANGUAGE sql VOLATILE;

CREATE TABLE IF NOT EXISTS billing.products (
    id           UUID PRIMARY KEY,
    slug         TEXT NOT NULL,
    display_name TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'active',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'openrails_app') THEN
        CREATE ROLE openrails_app LOGIN PASSWORD 'app_pw' NOBYPASSRLS;
    END IF;
END $$;
GRANT USAGE ON SCHEMA billing TO openrails_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON billing.products TO openrails_app;
`

func pagStartContainer(t *testing.T) (superDSN, appDSN string) {
	t.Helper()
	ctx := context.Background()

	// Escape hatch for flaky-testcontainers hosts: OPENRAILS_TEST_DB_DSN is the
	// SUPER/admin DSN; the openrails_app DSN is derived by swapping userinfo (the
	// prereq DDL creates that role with password 'app_pw').
	if dsn := strings.TrimSpace(os.Getenv("OPENRAILS_TEST_DB_DSN")); dsn != "" {
		u, err := url.Parse(dsn)
		require.NoError(t, err)
		superDSN = dsn
		u.User = url.UserPassword("openrails_app", "app_pw")
		appDSN = u.String()
		return superDSN, appDSN
	}

	container, err := postgres.Run(ctx,
		"postgres:18-alpine",
		postgres.WithDatabase("openrails"),
		postgres.WithUsername("super"),
		postgres.WithPassword("super"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	host, err := container.Host(ctx)
	require.NoError(t, err)
	port, err := container.MappedPort(ctx, "5432/tcp")
	require.NoError(t, err)

	superDSN = fmt.Sprintf("postgresql://super:super@%s:%s/openrails?sslmode=disable", host, port.Port())
	appDSN = fmt.Sprintf("postgresql://openrails_app:app_pw@%s:%s/openrails?sslmode=disable", host, port.Port())
	return superDSN, appDSN
}

func pagOpenBun(t *testing.T, dsn string) *bun.DB {
	t.Helper()
	sqlDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	bunDB := bun.NewDB(sqlDB, pgdialect.New())
	models.RegisterModels(bunDB)
	require.NoError(t, bunDB.PingContext(context.Background()))
	t.Cleanup(func() { _ = bunDB.Close() })
	return bunDB
}

func pagOpenDB(t *testing.T, dsn string) *db.DB {
	t.Helper()
	dbi, err := db.NewWithBun(pagOpenBun(t, dsn))
	require.NoError(t, err)
	return dbi
}

func TestProductAccessGrants_RealMigration_RLS(t *testing.T) {
	ctx := context.Background()
	superDSN, appDSN := pagStartContainer(t)

	// As superuser: prereqs + the REAL migration 063 up SQL.
	superBun := pagOpenBun(t, superDSN)
	_, err := superBun.ExecContext(ctx, pagPrereqDDL)
	require.NoError(t, err)
	up063, err := postgresmigrations.FS.ReadFile("063_product_access_grants.up.sql")
	require.NoError(t, err)
	_, err = superBun.ExecContext(ctx, string(up063))
	require.NoError(t, err)

	// Seed a product per tenant (superuser bypasses RLS so it can write any tenant).
	productA := uuid.New()
	productB := uuid.New()
	for _, p := range []struct {
		id   uuid.UUID
		slug string
	}{{productA, "prod-a-" + productA.String()}, {productB, "prod-b-" + productB.String()}} {
		// Explicit columns: the minimal pagPrereqDDL products table only has the
		// FK-target columns, not the full models.Product column set.
		_, err = superBun.NewRaw(
			`INSERT INTO billing.products (id, slug, display_name, status) VALUES (?, ?, 'P', 'active')`,
			p.id, p.slug,
		).Exec(ctx)
		require.NoError(t, err)
	}

	// App connection (openrails_app) -> RLS enforces.
	appDB := pagOpenDB(t, appDSN)
	now := time.Now().UTC().Truncate(time.Second)
	svc := NewService(appDB, clockwork.NewFakeClockAt(now))

	userID := uuid.New().String()
	paymentID := uuid.New()

	ctxA := tenant.WithID(ctx, pagTenantA)
	ctxB := tenant.WithID(ctx, pagTenantB)

	// (1) Purchase -> grant created (tenant A).
	_, created, err := svc.GrantProductAccess(ctxA, GrantParams{
		UserID:     userID,
		ProductID:  productA,
		SourceType: models.ProductAccessSourcePurchase,
		SourceID:   paymentID.String(),
		PaymentID:  &paymentID,
	})
	require.NoError(t, err)
	require.True(t, created)

	has, err := svc.HasProductAccess(ctxA, userID, productA)
	require.NoError(t, err)
	require.True(t, has)

	// (2) Duplicate purchase -> still one grant.
	_, created2, err := svc.GrantProductAccess(ctxA, GrantParams{
		UserID:     userID,
		ProductID:  productA,
		SourceType: models.ProductAccessSourcePurchase,
		SourceID:   paymentID.String(),
		PaymentID:  &paymentID,
	})
	require.NoError(t, err)
	require.False(t, created2)
	grantsA, err := svc.ListAccessibleProducts(ctxA, userID)
	require.NoError(t, err)
	require.Len(t, grantsA, 1, "duplicate purchase must yield exactly one grant")

	// (3) Tenant isolation: tenant B cannot see tenant A's grant.
	hasB, err := svc.HasProductAccess(ctxB, userID, productA)
	require.NoError(t, err)
	require.False(t, hasB, "tenant B must not see tenant A's grant")
	grantsB, err := svc.ListAccessibleProducts(ctxB, userID)
	require.NoError(t, err)
	require.Len(t, grantsB, 0, "tenant B's library must be empty")

	// (4) Refund -> grant revoked (tenant A). And it is invisible to B's revoke.
	nB, err := svc.RevokeProductAccessByPayment(ctxB, paymentID, models.ProductAccessRevokeRefund)
	require.NoError(t, err)
	require.Equal(t, int64(0), nB, "tenant B cannot revoke tenant A's grant")

	nA, err := svc.RevokeProductAccessByPayment(ctxA, paymentID, models.ProductAccessRevokeRefund)
	require.NoError(t, err)
	require.Equal(t, int64(1), nA)

	hasAfter, err := svc.HasProductAccess(ctxA, userID, productA)
	require.NoError(t, err)
	require.False(t, hasAfter, "after refund tenant A has no access")
}
