//go:build integration

package crypto

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) { dbtest.RunMain(m) }

func startCryptoPostgres(t *testing.T) (*db.Pool, context.Context) {
	t.Helper()
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	rawPool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(rawPool.Close)
	return db.WrapPool(rawPool, config.DefaultSchema), ctx
}

// countDEKs reads merchant_deks the way production does — inside the merchant's
// own pinned transaction. The raw pool is RLS-enforcing with no app.merchant_id,
// so a bare SELECT here would report 0 whether or not the row exists.
func countDEKs(t *testing.T, ctx context.Context, pool *db.Pool, m merchant.ID) int {
	t.Helper()
	var n int
	require.NoError(t, pool.MerchantTx(ctx, m, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM openrails.merchant_deks WHERE merchant_id=$1::uuid`, m.String()).Scan(&n)
	}))
	return n
}

// seedMerchant inserts a merchants row so merchant_deks_merchant_fk is satisfied.
func seedMerchant(t *testing.T, ctx context.Context, pool *db.Pool) merchant.ID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(ctx,
		`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`,
		id, "crypto-"+id.String())
	require.NoError(t, err)
	return merchant.ID(id)
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

	tA := seedMerchant(t, ctx, pool)

	// No DEK row before first use.
	require.Equal(t, 0, countDEKs(t, ctx, pool, tA))

	ct, err := enc.Encrypt(ctx, tA, testAAD(tA), []byte("sk_live_db"))
	require.NoError(t, err)

	// Exactly one wrapped DEK row created lazily.
	require.Equal(t, 1, countDEKs(t, ctx, pool, tA))

	// Round-trip with a FRESH encryptor (cold cache) over the same store+master
	// key: it must unwrap the persisted DEK and decrypt.
	enc2, err := NewEncryptor(mk, store)
	require.NoError(t, err)
	got, err := enc2.Decrypt(ctx, tA, testAAD(tA), ct)
	require.NoError(t, err)
	require.Equal(t, "sk_live_db", string(got))

	// Re-encrypting reuses the SAME DEK row (no second row).
	_, err = enc.Encrypt(ctx, tA, testAAD(tA), []byte("again"))
	require.NoError(t, err)
	require.Equal(t, 1, countDEKs(t, ctx, pool, tA), "DEK must be reused, not recreated")
}

func TestDBDEKStore_CrossMerchantCiphertextIsolation(t *testing.T) {
	pool, ctx := startCryptoPostgres(t)
	store, _ := NewDBDEKStore(pool)
	enc, _ := NewEncryptor(masterKey(t), store)

	tA := seedMerchant(t, ctx, pool)
	tB := seedMerchant(t, ctx, pool)
	ctA, err := enc.Encrypt(ctx, tA, testAAD(tA), []byte("A-only"))
	require.NoError(t, err)
	_, err = enc.Decrypt(ctx, tB, testAAD(tB), ctA)
	require.Error(t, err, "merchant B DEK must not decrypt merchant A ciphertext")
}
