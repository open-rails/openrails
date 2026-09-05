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

func TestDBSecretReadsReuseOneConnectionAndKeepMerchantScope(t *testing.T) {
	ctx := context.Background()
	_, dsn := dbtest.SharedRLSPostgres(t)
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	defer pool.Close()
	database, err := db.NewWithPGXPool(pool, "openrails")
	require.NoError(t, err)
	store, err := NewDBSecretStore(database.DataPool())
	require.NoError(t, err)
	svc, err := NewService(database.DataPool(), store, "live")
	require.NoError(t, err)
	a, _, err := svc.Provision(ctx, ProvisionRequest{Slug: "secret-pin-a-" + uuid.NewString()[:8], PermissionGroupID: uuid.NewString()})
	require.NoError(t, err)
	b, _, err := svc.Provision(ctx, ProvisionRequest{Slug: "secret-pin-b-" + uuid.NewString()[:8], PermissionGroupID: uuid.NewString()})
	require.NoError(t, err)
	account := "pin-" + uuid.NewString()
	require.NoError(t, database.DataPool().MerchantTx(ctx, a.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO openrails.psps(merchant_id,rail,environment,account_id) VALUES($1,'stripe','live',$2)`, a.ID.UUID(), account)
		return err
	}))
	name, err := PSPSecretName("stripe", "live", account, "secret_key")
	require.NoError(t, err)
	_, err = store.Put(ctx, a.ID, name, "original-test-credential")
	require.NoError(t, err)
	_, err = store.Put(ctx, b.ID, name, "other-test-credential")
	require.NoError(t, err)
	// Bound the regression itself: the old read attempted a second connection
	// while this request held the only one, despite both reads targeting A.
	ctx, cancel := context.WithTimeout(merchant.WithID(ctx, a.ID), 3*time.Second)
	defer cancel()
	ctx, release, err := database.WithMerchantConn(ctx)
	require.NoError(t, err)
	defer release()
	_, err = database.Qx(ctx).Exec(ctx, "SELECT 1")
	require.NoError(t, err)
	secret, err := store.Get(ctx, a.ID, name)
	require.NoError(t, err)
	require.Equal(t, "original-test-credential", secret.Value)
	names, err := store.List(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, []string{name}, names)
	creds, err := svc.LoadStripeCredentials(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "original-test-credential", creds.SecretKey)
	_, err = store.Get(ctx, b.ID, name)
	require.Error(t, err, "the pinned merchant cannot read another merchant's secret")
	_, err = store.List(ctx, b.ID)
	require.Error(t, err, "the pinned merchant cannot enumerate another merchant's secrets")
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
