//go:build integration

package merchants

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestRestoreIdentityPreservesUUIDAndNeverRebinds(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbtest.SharedPostgresDSN(t))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	directory, err := NewDirectoryService(db.WrapPool(pool, config.DefaultSchema))
	require.NoError(t, err)
	id, group, slug := merchant.ID(uuid.New()), uuid.NewString(), "restore-"+uuid.NewString()[:8]
	req := ProvisionRequest{Slug: slug, PermissionGroupID: group}
	first, created, err := directory.ProvisionForRestore(ctx, id, req)
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, id, first.ID)
	again, created, err := directory.ProvisionForRestore(ctx, id, req)
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, first, again)

	t.Run("same group cannot acquire another billing UUID", func(t *testing.T) {
		otherID := merchant.ID(uuid.New())
		_, _, err := directory.ProvisionForRestore(ctx, otherID, req)
		require.ErrorIs(t, err, ErrMerchantRestoreConflict)
		_, err = directory.Get(ctx, otherID)
		require.ErrorIs(t, err, ErrMerchantNotFound)
	})
	t.Run("foreign UUID is never adopted by group or name", func(t *testing.T) {
		for _, wrong := range []ProvisionRequest{
			{Slug: slug, PermissionGroupID: uuid.NewString()},
			{Slug: slug + "-other", PermissionGroupID: group},
		} {
			_, _, err := directory.ProvisionForRestore(ctx, id, wrong)
			require.ErrorIs(t, err, ErrMerchantRestoreConflict)
		}
		_, _, err := directory.RegisterForRestore(ctx, id, slug)
		require.ErrorIs(t, err, ErrMerchantRestoreConflict)
		stored, err := directory.Get(ctx, id)
		require.NoError(t, err)
		require.Equal(t, first, stored)
	})
	t.Run("explicit unbound identity is stable", func(t *testing.T) {
		unboundID, unboundSlug := merchant.ID(uuid.New()), "unbound-"+uuid.NewString()[:8]
		unbound, made, err := directory.RegisterForRestore(ctx, unboundID, unboundSlug)
		require.NoError(t, err)
		require.True(t, made)
		require.Empty(t, unbound.PermissionGroupID)
		_, made, err = directory.RegisterForRestore(ctx, unboundID, unboundSlug)
		require.NoError(t, err)
		require.False(t, made)
		_, _, err = directory.RegisterForRestore(ctx, merchant.ID(uuid.New()), unboundSlug)
		require.ErrorIs(t, err, ErrMerchantRestoreConflict)
		_, _, err = directory.ProvisionForRestore(ctx, unboundID, ProvisionRequest{Slug: unboundSlug, PermissionGroupID: uuid.NewString()})
		require.ErrorIs(t, err, ErrMerchantRestoreConflict)
		normal, err := db.RegisterUnboundMerchant(ctx, db.WrapPool(pool, config.DefaultSchema), db.RegisterUnboundMerchantOptions{Slug: unboundSlug})
		require.NoError(t, err)
		require.Equal(t, unboundID, normal, "ordinary manifest registration keeps the restored identity")
	})
	t.Run("invalid or retired destination refuses", func(t *testing.T) {
		_, _, err := directory.ProvisionForRestore(ctx, merchant.ID{}, req)
		require.ErrorContains(t, err, "merchant_id is required")
		_, _, err = directory.ProvisionForRestore(ctx, merchant.ID(uuid.New()), ProvisionRequest{Slug: slug})
		require.ErrorIs(t, err, ErrPermissionGroupRequired)
		_, _, err = directory.RegisterForRestore(ctx, merchant.ID(uuid.New()), "")
		require.Error(t, err)
		_, err = pool.Exec(ctx, `UPDATE openrails.merchants SET status='deleted', deleted_at=now() WHERE id=$1`, id.UUID())
		require.NoError(t, err)
		_, _, err = directory.ProvisionForRestore(ctx, id, req)
		require.ErrorIs(t, err, ErrMerchantRestoreConflict)
	})
}

func TestConcurrentRestoreIdentityCreatesOnce(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbtest.SharedPostgresDSN(t))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	directory, err := NewDirectoryService(db.WrapPool(pool, config.DefaultSchema))
	require.NoError(t, err)
	id := merchant.ID(uuid.New())
	req := ProvisionRequest{Slug: "restore-race-" + uuid.NewString()[:8], PermissionGroupID: uuid.NewString()}
	const callers = 4
	var wg sync.WaitGroup
	results := make([]*Merchant, callers)
	created := make([]bool, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	for i := range callers {
		wg.Go(func() {
			<-start
			results[i], created[i], errs[i] = directory.ProvisionForRestore(ctx, id, req)
		})
	}
	close(start)
	wg.Wait()
	count := 0
	for i := range callers {
		require.NoError(t, errs[i])
		require.Equal(t, id, results[i].ID)
		if created[i] {
			count++
		}
	}
	require.Equal(t, 1, count)
}
