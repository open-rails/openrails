//go:build integration

package repo

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/tenant"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

func TestEnsureTenantSubjectID_UUIDReusesExistingPayableID(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)

	sqlDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { _ = sqlDB.Close() })
	bunDB := bun.NewDB(sqlDB, pgdialect.New())
	models.RegisterModels(bunDB)

	ctx := context.Background()
	require.NoError(t, bunDB.PingContext(ctx))

	tenantID := tenant.DefaultID.UUID()
	userID := uuid.New()
	createdAt := time.Now().UTC().Add(-time.Hour)
	_, err := bunDB.NewRaw(
		`INSERT INTO billing.tenant_subjects (id, tenant_id, issuer, subject, created_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		userID, tenantID, "http://doujins:2052", userID.String(), createdAt, createdAt,
	).Exec(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = bunDB.NewDelete().
			Model((*models.TenantSubject)(nil)).
			Where("id = ?", userID).
			ForceDelete().
			Exec(ctx)
	})

	resolved, err := EnsureTenantSubjectID(ctx, dbtest.SharedPGXPool(t), tenantID, userID.String())
	require.NoError(t, err)
	require.Equal(t, userID, resolved)

	var row models.TenantSubject
	require.NoError(t, bunDB.NewSelect().
		Model(&row).
		Where("id = ?", userID).
		Scan(ctx))
	require.Equal(t, "http://doujins:2052", row.Issuer)
	require.True(t, row.LastSeenAt.After(createdAt))
}
