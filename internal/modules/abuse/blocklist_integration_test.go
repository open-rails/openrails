//go:build integration

package abuse_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
)

// blocklistEnv opens the shared migrated Postgres, registers models, and returns
// a BlocklistService plus the raw bun.DB (for cleanup). All operations run under
// the default tenant via tenant.FromContextOrDefault, exactly like the credits
// integration harness.
func blocklistEnv(t *testing.T) (*abuse.BlocklistService, *bun.DB, context.Context) {
	t.Helper()
	dsn := dbtest.SharedPostgresDSN(t)
	sqlDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { _ = sqlDB.Close() })
	bunDB := bun.NewDB(sqlDB, pgdialect.New())
	models.RegisterModels(bunDB)
	ctx := context.Background()
	require.NoError(t, bunDB.PingContext(ctx))

	var hasTable bool
	require.NoError(t, bunDB.NewSelect().
		ColumnExpr("EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='billing' AND table_name='payment_blocklist')").
		Scan(ctx, &hasTable))
	if !hasTable {
		t.Skip("billing.payment_blocklist missing; run migration 067")
	}

	dbi, err := db.NewWithBun(bunDB)
	require.NoError(t, err)

	return abuse.NewBlocklistService(dbi), bunDB, ctx
}

func TestBlocklist_TenantWideAddIsBlockedRemove(t *testing.T) {
	svc, bunDB, ctx := blocklistEnv(t)

	// Use unique values so the shared (persistent) container doesn't collide
	// across test runs.
	value := "fp_" + uuid.NewString()
	other := "fp_" + uuid.NewString()
	t.Cleanup(func() {
		_, _ = bunDB.NewDelete().Model((*models.PaymentBlocklistEntry)(nil)).
			Where("value IN (?, ?)", value, other).Exec(context.Background())
	})

	// Not blocked before adding.
	blocked, err := svc.IsBlocked(ctx, abuse.KindCardFingerprint, value)
	require.NoError(t, err)
	require.False(t, blocked)

	// Add a TENANT-WIDE block (owner nil).
	require.NoError(t, svc.Add(ctx, nil, abuse.KindCardFingerprint, value, "chargeback ring"))

	// Blocked for that (kind, value).
	blocked, err = svc.IsBlocked(ctx, abuse.KindCardFingerprint, value)
	require.NoError(t, err)
	require.True(t, blocked)

	// Not blocked for a DIFFERENT value.
	blocked, err = svc.IsBlocked(ctx, abuse.KindCardFingerprint, other)
	require.NoError(t, err)
	require.False(t, blocked)

	// Not blocked for the same value under a DIFFERENT kind.
	blocked, err = svc.IsBlocked(ctx, abuse.KindEmail, value)
	require.NoError(t, err)
	require.False(t, blocked)

	// Remove -> no longer blocked.
	require.NoError(t, svc.Remove(ctx, abuse.KindCardFingerprint, value))
	blocked, err = svc.IsBlocked(ctx, abuse.KindCardFingerprint, value)
	require.NoError(t, err)
	require.False(t, blocked)
}

func TestBlocklist_OwnerScopedAddIsBlocked(t *testing.T) {
	svc, bunDB, ctx := blocklistEnv(t)

	value := "cust_" + uuid.NewString()
	t.Cleanup(func() {
		_, _ = bunDB.NewDelete().Model((*models.PaymentBlocklistEntry)(nil)).
			Where("value = ?", value).Exec(context.Background())
	})

	owner := identity.TenantSubjectIDFromString(uuid.NewString())

	// Not blocked before adding.
	blocked, err := svc.IsBlocked(ctx, abuse.KindProcessorCustomer, value)
	require.NoError(t, err)
	require.False(t, blocked)

	// Add an OWNER-SCOPED block.
	require.NoError(t, svc.Add(ctx, &owner, abuse.KindProcessorCustomer, value, "fraud"))

	// IsBlocked is true (matches any owner-scoped row for this kind+value).
	blocked, err = svc.IsBlocked(ctx, abuse.KindProcessorCustomer, value)
	require.NoError(t, err)
	require.True(t, blocked)

	// Verify the stored row actually carries the owner scope.
	entry := new(models.PaymentBlocklistEntry)
	require.NoError(t, bunDB.NewSelect().Model(entry).
		Where("kind = ? AND value = ?", abuse.KindProcessorCustomer, value).
		Limit(1).Scan(ctx))
	require.NotNil(t, entry.OwnerID)
	require.Equal(t, owner.UUID(), *entry.OwnerID)

	// Remove clears it.
	require.NoError(t, svc.Remove(ctx, abuse.KindProcessorCustomer, value))
	blocked, err = svc.IsBlocked(ctx, abuse.KindProcessorCustomer, value)
	require.NoError(t, err)
	require.False(t, blocked)
}

func TestBlocklist_AddIsIdempotent(t *testing.T) {
	svc, bunDB, ctx := blocklistEnv(t)

	value := "ip_" + uuid.NewString()
	t.Cleanup(func() {
		_, _ = bunDB.NewDelete().Model((*models.PaymentBlocklistEntry)(nil)).
			Where("value = ?", value).Exec(context.Background())
	})

	require.NoError(t, svc.Add(ctx, nil, abuse.KindIP, value, "abuse"))
	require.NoError(t, svc.Add(ctx, nil, abuse.KindIP, value, "abuse again"))

	count, err := bunDB.NewSelect().Model((*models.PaymentBlocklistEntry)(nil)).
		Where("kind = ? AND value = ?", abuse.KindIP, value).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

func TestBlocklist_InvalidKind(t *testing.T) {
	svc, _, ctx := blocklistEnv(t)

	require.ErrorIs(t, svc.Add(ctx, nil, "nope", "x", ""), abuse.ErrInvalidKind)
	_, err := svc.IsBlocked(ctx, "nope", "x")
	require.ErrorIs(t, err, abuse.ErrInvalidKind)
	require.ErrorIs(t, svc.Remove(ctx, "nope", "x"), abuse.ErrInvalidKind)
}
