//go:build integration

package productaccess

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	postgresmigrations "github.com/open-rails/openrails/migrations/postgres"
	"github.com/open-rails/openrails/pkg/merchant"
)

// This suite proves the consolidated product_access_grants table + its RLS
// policy enforce durable ownership AND tenant isolation
// when the app connects as the unprivileged openrails_app role (issue #227/#250).
// It runs the REAL consolidated Postgres migrations, then drives
// the productaccess.Service through db.RunInTenantTx (which pins app.merchant_id),
// so it validates the real schema, the real policy, and the GUC plumbing.
//
// Asserts: purchase -> grant created; duplicate purchase -> still one grant;
// refund -> grant revoked; tenant isolation (tenant A's grant invisible to B).

var (
	pagTenantA = mustTenant("00000000-0000-0000-0000-0000000000a1")
	pagTenantB = mustTenant("00000000-0000-0000-0000-0000000000b2")
)

func mustTenant(s string) merchant.ID {
	id, err := merchant.ParseID(s)
	if err != nil {
		panic(err)
	}
	return id
}

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

func pagOpenPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	require.NoError(t, err)
	require.NoError(t, pool.Ping(context.Background()))
	t.Cleanup(pool.Close)
	return pool
}

func pagOpenDB(t *testing.T, dsn string) *db.DB {
	t.Helper()
	return dbtest.OpenAppDB(t, dsn)
}

func TestProductAccessGrants_RealMigration_RLS(t *testing.T) {
	ctx := context.Background()
	superDSN, appDSN := pagStartContainer(t)

	// As superuser: the REAL consolidated Postgres migrations.
	superPool := pagOpenPool(t, superDSN)
	schema001, err := postgresmigrations.FS.ReadFile("001_schema.up.sql")
	require.NoError(t, err)
	_, err = superPool.Exec(ctx, string(schema001))
	require.NoError(t, err)
	seed002, err := postgresmigrations.FS.ReadFile("002_seed.up.sql")
	require.NoError(t, err)
	_, err = superPool.Exec(ctx, string(seed002))
	require.NoError(t, err)
	_, err = superPool.Exec(ctx, `ALTER ROLE openrails_app LOGIN PASSWORD 'app_pw'`)
	require.NoError(t, err)
	for _, tt := range []struct {
		id   merchant.ID
		slug string
	}{{pagTenantA, "tenant-a"}, {pagTenantB, "tenant-b"}} {
		_, err = superPool.Exec(ctx,
			`INSERT INTO openrails.merchants (id, slug, name, status) VALUES ($1, $2, $3, 'active') ON CONFLICT (slug) DO NOTHING`,
			tt.id.UUID(), tt.slug, tt.slug,
		)
		require.NoError(t, err)
	}

	// Seed a product per tenant (superuser bypasses RLS so it can write any tenant).
	productA := uuid.New()
	productB := uuid.New()
	for _, p := range []struct {
		id   uuid.UUID
		slug string
	}{{productA, "prod-a-" + productA.String()}, {productB, "prod-b-" + productB.String()}} {
		// Explicit columns keep the fixture focused on the product-access FK target.
		tenantID := pagTenantA.UUID()
		if p.id == productB {
			tenantID = pagTenantB.UUID()
		}
		_, err = superPool.Exec(ctx,
			`INSERT INTO openrails.products (id, merchant_id, slug, display_name, status) VALUES ($1, $2, $3, 'P', 'active')`,
			p.id, tenantID, p.slug,
		)
		require.NoError(t, err)
	}

	// App connection (openrails_app) -> RLS enforces.
	appDB := pagOpenDB(t, appDSN)
	now := time.Now().UTC().Truncate(time.Second)
	svc := NewService(appDB, clockwork.NewFakeClockAt(now))

	userID := uuid.New().String()
	paymentID := uuid.New()

	ctxA := merchant.WithID(ctx, pagTenantA)
	ctxB := merchant.WithID(ctx, pagTenantB)

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
