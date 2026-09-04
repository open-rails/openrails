//go:build integration

package merchants

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/stretchr/testify/require"
)

func TestProvisionKeepsImmutableGroupOwnership(t *testing.T) {
	ctx := context.Background()
	_, appDSN := dbtest.SharedRLSPostgres(t)
	raw, err := pgxpool.New(ctx, appDSN)
	require.NoError(t, err)
	defer raw.Close()
	svc, err := NewDirectoryService(db.WrapPool(raw, config.DefaultSchema))
	require.NoError(t, err)
	slug := "binding-" + uuid.NewString()[:8]
	group := uuid.NewString()
	first, created, err := svc.Provision(ctx, ProvisionRequest{Slug: slug, PermissionGroupID: group})
	require.NoError(t, err)
	require.True(t, created)

	t.Run("different group cannot steal a live name", func(t *testing.T) {
		_, _, err := svc.Provision(ctx, ProvisionRequest{Slug: slug, PermissionGroupID: uuid.NewString()})
		require.ErrorIs(t, err, ErrMerchantBindingConflict)
		got, err := svc.Get(ctx, first.ID)
		require.NoError(t, err)
		require.Equal(t, group, got.PermissionGroupID)
	})
	t.Run("same group survives a rename", func(t *testing.T) {
		got, created, err := svc.Provision(ctx, ProvisionRequest{Slug: slug + "-new", PermissionGroupID: group})
		require.NoError(t, err)
		require.False(t, created)
		require.Equal(t, first.ID, got.ID)
	})
	t.Run("database refuses rebinding", func(t *testing.T) {
		_, err := raw.Exec(ctx, `UPDATE openrails.merchants SET permission_group_id=$2 WHERE id=$1`, first.ID.UUID(), uuid.NewString())
		require.ErrorContains(t, err, "merchant group binding is immutable")
	})
	t.Run("concurrent same-group provisions converge", func(t *testing.T) {
		var wg sync.WaitGroup
		results := make(chan *Merchant, 8)
		failures := make(chan error, 8)
		var createdCount int
		var mu sync.Mutex
		freshGroup, freshSlug := uuid.NewString(), "concurrent-"+uuid.NewString()[:8]
		for range 8 {
			wg.Go(func() {
				got, made, err := svc.Provision(ctx, ProvisionRequest{Slug: freshSlug, PermissionGroupID: freshGroup})
				if err != nil {
					failures <- err
					return
				}
				if made {
					mu.Lock()
					createdCount++
					mu.Unlock()
				}
				results <- got
			})
		}
		wg.Wait()
		close(results)
		close(failures)
		for err := range failures {
			require.NoError(t, err)
		}
		require.Equal(t, 1, createdCount)
		var id string
		for got := range results {
			if id == "" {
				id = got.ID.String()
			}
			require.Equal(t, id, got.ID.String())
		}
	})
	t.Run("retired binding cannot create a new billing identity", func(t *testing.T) {
		_, err := raw.Exec(ctx, `UPDATE openrails.merchants SET status='deleted',deleted_at=now() WHERE id=$1`, first.ID.UUID())
		require.NoError(t, err)
		_, _, err = svc.Provision(ctx, ProvisionRequest{Slug: slug + "-again", PermissionGroupID: group})
		require.ErrorIs(t, err, ErrMerchantRetired)
		other, made, err := svc.Provision(ctx, ProvisionRequest{Slug: slug, PermissionGroupID: uuid.NewString()})
		require.NoError(t, err)
		require.True(t, made)
		require.NotEqual(t, first.ID, other.ID)
	})
}
