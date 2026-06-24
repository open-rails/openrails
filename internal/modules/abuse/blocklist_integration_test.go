//go:build integration

package abuse_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/stretchr/testify/require"
)

// blocklistEnv opens the shared migrated Postgres and returns a
// BlocklistService plus the raw pgx pool (for cleanup/assertions). All
// operations run under the canonical test merchant (merchant.Require),
// exactly like the credits integration harness.
func blocklistEnv(t *testing.T) (*abuse.BlocklistService, *pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := dbtest.SharedPostgresDSN(t)
	ctx := dbtest.WithTestMerchant(context.Background())

	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(ctx, t, pool)

	var hasTable bool
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = $1 AND table_name='payment_blocklist')",
		dbi.DataPool().Schema(),
	).Scan(&hasTable))
	if !hasTable {
		t.Skip("payment_blocklist missing in the configured schema; run migrations before integration tests")
	}

	return abuse.NewBlocklistService(dbi), pool, ctx
}

func TestBlocklist_MerchantWideAddIsBlockedRemove(t *testing.T) {
	svc, pool, ctx := blocklistEnv(t)

	// Use unique values so the shared (persistent) container doesn't collide
	// across test runs.
	value := "fp_" + uuid.NewString()
	other := "fp_" + uuid.NewString()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM openrails.payment_blocklist WHERE value IN ($1, $2)", value, other)
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
	svc, pool, ctx := blocklistEnv(t)

	value := "cust_" + uuid.NewString()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM openrails.payment_blocklist WHERE value = $1", value)
	})

	owner := identity.CustomerIDFromString(uuid.NewString())

	// Not blocked before adding.
	blocked, err := svc.IsBlocked(ctx, abuse.KindRailCustomer, value)
	require.NoError(t, err)
	require.False(t, blocked)

	// Add an OWNER-SCOPED block.
	require.NoError(t, svc.Add(ctx, &owner, abuse.KindRailCustomer, value, "fraud"))

	// IsBlocked is true (matches any owner-scoped row for this kind+value).
	blocked, err = svc.IsBlocked(ctx, abuse.KindRailCustomer, value)
	require.NoError(t, err)
	require.True(t, blocked)

	// Verify the stored row actually carries the owner scope.
	var storedOwner *uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT customer_id FROM openrails.payment_blocklist WHERE kind = $1 AND value = $2 LIMIT 1",
		abuse.KindRailCustomer, value,
	).Scan(&storedOwner))
	require.NotNil(t, storedOwner)
	require.Equal(t, owner.UUID(), *storedOwner)

	// Remove clears it.
	require.NoError(t, svc.Remove(ctx, abuse.KindRailCustomer, value))
	blocked, err = svc.IsBlocked(ctx, abuse.KindRailCustomer, value)
	require.NoError(t, err)
	require.False(t, blocked)
}

func TestBlocklist_AddIsIdempotent(t *testing.T) {
	svc, pool, ctx := blocklistEnv(t)

	value := "ip_" + uuid.NewString()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM openrails.payment_blocklist WHERE value = $1", value)
	})

	require.NoError(t, svc.Add(ctx, nil, abuse.KindIP, value, "abuse"))
	require.NoError(t, svc.Add(ctx, nil, abuse.KindIP, value, "abuse again"))

	var count int
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT count(*) FROM openrails.payment_blocklist WHERE kind = $1 AND value = $2",
		abuse.KindIP, value,
	).Scan(&count))
	require.Equal(t, 1, count)
}

func TestBlocklist_InvalidKind(t *testing.T) {
	svc, _, ctx := blocklistEnv(t)

	require.ErrorIs(t, svc.Add(ctx, nil, "nope", "x", ""), abuse.ErrInvalidKind)
	_, err := svc.IsBlocked(ctx, "nope", "x")
	require.ErrorIs(t, err, abuse.ErrInvalidKind)
	require.ErrorIs(t, svc.Remove(ctx, "nope", "x"), abuse.ErrInvalidKind)
}
