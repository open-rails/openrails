//go:build integration

package merchants

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestDirectoryReadsReuseOneConnection(t *testing.T) {
	_, dsn := dbtest.SharedRLSPostgres(t)
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	require.NoError(t, err)
	defer pool.Close()
	database, err := db.NewWithPGXPool(pool, "openrails")
	require.NoError(t, err)
	directory, err := NewDirectoryService(database.DataPool())
	require.NoError(t, err)
	slug, group := "pin-"+uuid.NewString()[:8], uuid.NewString()
	row, _, err := directory.Provision(context.Background(), ProvisionRequest{Slug: slug, PermissionGroupID: group})
	require.NoError(t, err)
	directory.WithGroupSlugResolver(func(context.Context, string) (string, string, error) { return group, slug + "-new", nil })
	directory.WithGroupIDResolver(func(context.Context, string) (string, error) { return slug + "-new", nil })
	ctx, cancel := context.WithTimeout(merchant.WithID(context.Background(), row.ID), 3*time.Second)
	defer cancel()
	ctx, release, err := database.WithMerchantConn(ctx)
	require.NoError(t, err)
	defer release()
	// Force acquisition before directory reads so accidental second-pool reads hang.
	_, err = database.Qx(ctx).Exec(ctx, "SELECT 1")
	require.NoError(t, err)
	got, err := directory.Get(ctx, row.ID)
	require.NoError(t, err)
	require.Equal(t, row.ID, got.ID)
	got, err = directory.GetBySlug(ctx, slug)
	require.NoError(t, err)
	require.Equal(t, slug+"-new", got.Slug)
	current, err := directory.CanonicalSlug(ctx, row.ID)
	require.NoError(t, err)
	require.Equal(t, slug+"-new", current)
}

func TestRestoreRefusesCommittedPurgeButAllowsFailedPreflight(t *testing.T) {
	_, dsn := dbtest.SharedRLSPostgres(t)
	pool, err := pgxpool.New(context.Background(), dsn)
	require.NoError(t, err)
	defer pool.Close()
	wrapped := db.WrapPool(pool, "openrails")
	directory, err := NewDirectoryService(wrapped)
	require.NoError(t, err)
	for _, committed := range []bool{false, true} {
		row, _, err := directory.Provision(context.Background(), ProvisionRequest{Slug: "restore-" + uuid.NewString()[:8], PermissionGroupID: uuid.NewString()})
		require.NoError(t, err)
		require.NoError(t, wrapped.MerchantTx(context.Background(), row.ID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO openrails.destructive_runs(merchant_id,kind,actor,status,affected) VALUES($1,'merchant_purge','test','failed',jsonb_build_object('database_purged',$2::boolean))`, row.ID.UUID(), committed)
			if err != nil {
				return err
			}
			_, err = gen.New(tx).SoftDeletePlatformMerchant(ctx, row.ID.UUID())
			return err
		}))
		_, err = gen.New(wrapped).RestorePlatformMerchant(context.Background(), row.ID.UUID())
		if committed {
			require.ErrorContains(t, err, "cannot be restored")
		} else {
			require.NoError(t, err)
		}
	}
}
