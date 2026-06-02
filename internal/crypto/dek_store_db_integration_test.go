//go:build integration

package crypto

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/migratekit"
	postgresmigrations "github.com/open-rails/openrails/migrations/postgres"
	"github.com/open-rails/openrails/pkg/tenant"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/uptrace/bun/driver/pgdriver"
)

func startCryptoPostgres(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	ctx := context.Background()
	container, err := postgres.Run(ctx,
		"postgres:18-alpine",
		postgres.WithDatabase("test_db"),
		postgres.WithUsername("test_user"),
		postgres.WithPassword("test_password"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	sqlDB := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, sqlDB.PingContext(ctx))
	_, err = sqlDB.ExecContext(ctx, `
		CREATE SCHEMA IF NOT EXISTS billing;
		CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;
		CREATE EXTENSION IF NOT EXISTS btree_gist;
	`)
	require.NoError(t, err)
	migrations, err := migratekit.LoadFromFS(postgresmigrations.FS)
	require.NoError(t, err)
	require.NoError(t, migratekit.NewPostgres(sqlDB, "billing").ApplyMigrations(ctx, migrations))

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool, ctx
}

func masterKey(t *testing.T) string {
	t.Helper()
	k := make([]byte, keySize)
	_, err := io.ReadFull(rand.Reader, k)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(k)
}

func TestDBDEKStore_LazyCreateReuseAndRoundTrip(t *testing.T) {
	pool, ctx := startCryptoPostgres(t)
	store, err := NewDBDEKStore(pool)
	require.NoError(t, err)

	mk := masterKey(t)
	enc, err := NewEncryptor(mk, store)
	require.NoError(t, err)

	tA := tenant.ID(uuid.New())

	// No DEK row before first use.
	var before int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM billing.tenant_deks WHERE tenant_id=$1::uuid`, tA.String()).Scan(&before))
	require.Equal(t, 0, before)

	ct, err := enc.Encrypt(ctx, tA, []byte("sk_live_db"))
	require.NoError(t, err)

	// Exactly one wrapped DEK row created lazily.
	var after int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM billing.tenant_deks WHERE tenant_id=$1::uuid`, tA.String()).Scan(&after))
	require.Equal(t, 1, after)

	// Round-trip with a FRESH encryptor (cold cache) over the same store+master
	// key: it must unwrap the persisted DEK and decrypt.
	enc2, err := NewEncryptor(mk, store)
	require.NoError(t, err)
	got, err := enc2.Decrypt(ctx, tA, ct)
	require.NoError(t, err)
	require.Equal(t, "sk_live_db", string(got))

	// Re-encrypting reuses the SAME DEK row (no second row).
	_, err = enc.Encrypt(ctx, tA, []byte("again"))
	require.NoError(t, err)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM billing.tenant_deks WHERE tenant_id=$1::uuid`, tA.String()).Scan(&after))
	require.Equal(t, 1, after, "DEK must be reused, not recreated")
}

func TestDBDEKStore_CrossTenantCiphertextIsolation(t *testing.T) {
	pool, ctx := startCryptoPostgres(t)
	store, _ := NewDBDEKStore(pool)
	enc, _ := NewEncryptor(masterKey(t), store)

	tA := tenant.ID(uuid.New())
	tB := tenant.ID(uuid.New())
	ctA, err := enc.Encrypt(ctx, tA, []byte("A-only"))
	require.NoError(t, err)
	_, err = enc.Decrypt(ctx, tB, ctA)
	require.Error(t, err, "tenant B DEK must not decrypt tenant A ciphertext")
}
