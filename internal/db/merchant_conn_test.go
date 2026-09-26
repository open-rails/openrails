package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

// Pins are lazy, so these ownership checks need no live connection.
func TestMerchantPinBelongsToItsDatabaseSchemaAndMerchant(t *testing.T) {
	firstPool, otherPool := new(pgxpool.Pool), new(pgxpool.Pool)
	first, err := NewWithPGXPool(firstPool, "openrails")
	require.NoError(t, err)
	other, err := NewWithPGXPool(otherPool, "openrails")
	require.NoError(t, err)
	_, err = NewWithPGXPool(nil, "openrails")
	require.Error(t, err)

	_, _, err = first.WithMerchantConn(context.Background())
	require.Error(t, err, "a pin needs a merchant")

	scope := merchant.ID(uuid.New())
	ctx, release, err := first.WithMerchantConn(merchant.WithID(context.Background(), scope))
	require.NoError(t, err)
	defer release()
	nestedCtx, _, err := first.WithMerchantConn(ctx)
	require.NoError(t, err)
	require.Equal(t, ctx, nestedCtx, "a compatible nested pin reuses the outer one")

	require.Equal(t, pooledDBTX{pool: otherPool, schema: "openrails"}, other.Qx(ctx), "another database must never execute on the first pool's pin")
	otherCtx, done, err := other.WithMerchantConn(ctx)
	require.NoError(t, err)
	defer done()
	require.NotSame(t, first.Qx(ctx), other.Qx(otherCtx))

	mismatch := merchant.WithID(ctx, merchant.ID(uuid.New()))
	_, err = first.Qx(mismatch).Exec(mismatch, "SELECT 1")
	require.ErrorContains(t, err, "does not match")
	_, _, err = first.WithMerchantConn(mismatch)
	require.ErrorContains(t, err, "does not match")
	relocated, err := NewWithPGXPool(firstPool, "another")
	require.NoError(t, err)
	_, err = relocated.Qx(ctx).Exec(ctx, "SELECT 1")
	require.ErrorContains(t, err, "does not match")
}

func TestIndependentMerchantPinKeepsAuthorityAndCancellation(t *testing.T) {
	database := &DB{pool: &pgxpool.Pool{}, rw: newSchemaRewriter("openrails")}
	mid := merchant.ID(uuid.New())
	ctx, cancel := context.WithCancel(merchant.WithID(context.Background(), mid))
	defer cancel()
	pinned, release, err := database.WithMerchantConn(ctx)
	require.NoError(t, err)
	defer release()
	fresh, releaseFresh, err := database.WithIndependentMerchantConn(pinned)
	require.NoError(t, err)
	defer releaseFresh()
	require.NotSame(t, pinned.Value(merchantPgxConnKey{}), fresh.Value(merchantPgxConnKey{}))
	got, err := merchant.Require(fresh)
	require.NoError(t, err)
	require.Equal(t, mid, got)
	cancel()
	require.ErrorIs(t, fresh.Err(), context.Canceled)

	for name, tc := range map[string]struct {
		db  *DB
		ctx context.Context
	}{
		"different pool":      {&DB{pool: &pgxpool.Pool{}, rw: newSchemaRewriter("openrails")}, pinned},
		"different schema":    {&DB{pool: database.pool, rw: newSchemaRewriter("other")}, pinned},
		"different merchant":  {database, merchant.WithID(pinned, merchant.ID(uuid.New()))},
		"no merchant":         {database, context.Background()},
		"transaction wrapper": {NewWithPgxTx(&recordingTx{}), pinned},
		"nil database":        {nil, pinned},
	} {
		_, cleanup, err := tc.db.WithIndependentMerchantConn(tc.ctx)
		cleanup()
		require.Error(t, err, name)
	}
}
