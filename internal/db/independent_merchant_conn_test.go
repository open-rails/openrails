package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestIndependentMerchantPinPreservesAuthorityAndCancellation(t *testing.T) {
	// Creation is lazy: no database connection is needed until the first query.
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

	for _, tc := range []struct {
		name string
		db   *DB
		ctx  context.Context
	}{
		{"different_pool", &DB{pool: &pgxpool.Pool{}, rw: newSchemaRewriter("openrails")}, pinned},
		{"different_schema", &DB{pool: database.pool, rw: newSchemaRewriter("other")}, pinned},
		{"different_merchant", database, merchant.WithID(pinned, merchant.ID(uuid.New()))},
		{"no_merchant", database, context.Background()},
		{"transaction_wrapper", NewWithPgxTx(nil), pinned},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, cleanup, err := tc.db.WithIndependentMerchantConn(tc.ctx)
			defer cleanup()
			require.Error(t, err)
		})
	}
}
