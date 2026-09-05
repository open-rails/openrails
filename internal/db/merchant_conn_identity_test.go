package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestMerchantPinBelongsToItsDatabaseAndScope(t *testing.T) {
	firstPool, otherPool := new(pgxpool.Pool), new(pgxpool.Pool)
	first, err := NewWithPGXPool(firstPool, "openrails")
	require.NoError(t, err)
	other, err := NewWithPGXPool(otherPool, "openrails")
	require.NoError(t, err)
	scope := merchant.ID(uuid.New())
	ctx, release, err := first.WithMerchantConn(merchant.WithID(context.Background(), scope))
	require.NoError(t, err)
	defer release()
	// Lazy pins require no actual network connection for this ownership check.
	require.Same(t, otherPool, other.Qx(ctx), "another database must never execute on the first pool's pin")
	nested, done, err := other.WithMerchantConn(ctx)
	require.NoError(t, err)
	defer done()
	require.NotSame(t, first.Qx(ctx), other.Qx(nested))
	mismatch := merchant.WithID(ctx, merchant.ID(uuid.New()))
	_, err = first.Qx(mismatch).Exec(mismatch, "SELECT 1")
	require.ErrorContains(t, err, "does not match")
	_, _, err = first.WithMerchantConn(mismatch)
	require.ErrorContains(t, err, "does not match")
	schema, err := NewWithPGXPool(firstPool, "another")
	require.NoError(t, err)
	_, err = schema.Qx(ctx).Exec(ctx, "SELECT 1")
	require.ErrorContains(t, err, "does not match")
}
