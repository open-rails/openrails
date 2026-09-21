//go:build integration

package crypto

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"testing"
	"time"

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
			`SELECT count(*) FROM billing.merchant_deks WHERE merchant_id=$1::uuid`, m.String()).Scan(&n)
	}))
	return n
}

// seedMerchant inserts a merchants row so merchant_deks_merchant_fk is satisfied.
func seedMerchant(t *testing.T, ctx context.Context, pool *db.Pool) merchant.ID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(ctx,
		`INSERT INTO billing.merchants (id, slug, status) VALUES ($1, $2, 'active')`,
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

// A real request has already acquired its one merchant connection before it
// needs to encrypt a short-lived capture secret. DEK custody must commit on
// that idle pin, rather than deadlock waiting for a second pool slot.
func TestDBDEKStore_ColdRequestUsesOnlyPoolSlot(t *testing.T) {
	ctx := t.Context()
	configuration, err := pgxpool.ParseConfig(dbtest.SharedPostgresDSN(t))
	require.NoError(t, err)
	configuration.MaxConns = 1
	raw, err := pgxpool.NewWithConfig(ctx, configuration)
	require.NoError(t, err)
	t.Cleanup(raw.Close)
	database, err := db.NewWithPGXPool(raw, config.DefaultSchema)
	require.NoError(t, err)
	pool := database.DataPool()
	owner := seedMerchant(t, ctx, pool)
	store, err := NewDBDEKStore(pool)
	require.NoError(t, err)
	key := masterKey(t)
	encryptor, err := NewEncryptor(key, store)
	require.NoError(t, err)
	require.NoError(t, database.RunInMerchantConn(merchant.WithID(ctx, owner), func(request context.Context) error {
		var one int
		require.NoError(t, database.Qx(request).QueryRow(request, "SELECT 1").Scan(&one))
		require.EqualValues(t, 1, raw.Stat().AcquiredConns())
		bounded, cancel := context.WithTimeout(request, 2*time.Second)
		defer cancel()
		ciphertext, err := encryptor.Encrypt(bounded, owner, SecretAAD(owner, "checkout/session-A/sdk"), []byte("short-lived-sdk-authorization"))
		if err != nil {
			return err
		}
		fresh, err := NewEncryptor(key, store)
		require.NoError(t, err)
		plaintext, err := fresh.Decrypt(bounded, owner, SecretAAD(owner, "checkout/session-A/sdk"), ciphertext)
		require.NoError(t, err)
		require.Equal(t, "short-lived-sdk-authorization", string(plaintext))
		require.EqualValues(t, 1, raw.Stat().TotalConns())
		return nil
	}))
	require.Equal(t, 1, countDEKs(t, ctx, pool, owner))
}

func TestDBDEKStore_CallerRollbackCannotOwnCachedKey(t *testing.T) {
	pool, ctx := startCryptoPostgres(t)
	database, err := db.NewWithPGXPool(pool.Raw(), config.DefaultSchema)
	require.NoError(t, err)
	for _, entry := range []string{"db_merchant", "db_plain", "pool_merchant", "bound_transaction", "bound_connection"} {
		t.Run(entry, func(t *testing.T) {
			owner := seedMerchant(t, ctx, pool)
			scoped := merchant.WithID(t.Context(), owner)
			store, err := NewDBDEKStore(pool)
			require.NoError(t, err)
			key := masterKey(t)
			cold, err := NewEncryptor(key, store)
			require.NoError(t, err)
			rolledBack := errors.New("roll back caller domain")
			work := func(txctx context.Context, tx pgx.Tx) error {
				_, err := cold.Encrypt(txctx, owner, SecretAAD(owner, "capture/test"), []byte("must-not-escape"))
				require.ErrorIs(t, err, db.ErrCallerTransaction)
				require.Empty(t, cold.dekGCMs, "a caller-owned transaction cannot seed the key cache")
				_, err = tx.Exec(txctx, `INSERT INTO openrails.merchant_configurations(merchant_id,config) VALUES($1,'{"caller":"rollback"}')`, owner.UUID())
				require.NoError(t, err)
				return rolledBack
			}
			switch entry {
			case "db_merchant":
				err = database.MerchantTx(scoped, work)
			case "db_plain":
				err = database.RunInTx(scoped, func(c context.Context, tx pgx.Tx) error {
					_, e := tx.Exec(c, "SELECT set_config('app.merchant_id',$1,true)", owner.String())
					require.NoError(t, e)
					return work(c, tx)
				})
			case "pool_merchant":
				err = pool.MerchantTx(scoped, owner, work)
			default:
				tx, e := pool.Begin(scoped)
				require.NoError(t, e)
				bound, txdb, e := database.BindMerchantTx(scoped, tx, owner)
				require.NoError(t, e)
				if entry == "bound_connection" {
					bound, release, e := txdb.WithMerchantConn(bound)
					require.NoError(t, e)
					err = work(bound, tx)
					release()
				} else {
					err = work(bound, tx)
				}
				require.NoError(t, tx.Rollback(context.WithoutCancel(scoped)))
			}
			require.ErrorIs(t, err, rolledBack)
			require.Equal(t, 0, countDEKs(t, ctx, pool, owner))

			// Initialize custody outside the domain transaction. Warm crypto inside a
			// subsequently canceled/rolled-back domain can never create an orphan key.
			aad := SecretAAD(owner, "capture/test")
			_, err = cold.Encrypt(scoped, owner, aad, []byte("initialize"))
			require.NoError(t, err)
			canceled, cancel := context.WithCancel(scoped)
			err = database.MerchantTx(canceled, func(c context.Context, tx pgx.Tx) error {
				ciphertext, e := cold.Encrypt(c, owner, aad, []byte("caller-rolled-back"))
				require.NoError(t, e)
				_, e = tx.Exec(c, `INSERT INTO openrails.merchant_configurations(merchant_id,config) VALUES($1,jsonb_build_object('ciphertext',$2::text))`, owner.UUID(), ciphertext)
				require.NoError(t, e)
				cancel()
				return context.Canceled
			})
			require.ErrorIs(t, err, context.Canceled)
			canceledCold, err := NewEncryptor(key, store)
			require.NoError(t, err)
			_, err = canceledCold.Encrypt(canceled, owner, aad, []byte("canceled cold access"))
			require.Error(t, err)
			require.Empty(t, canceledCold.dekGCMs)
			ciphertext, err := cold.Encrypt(scoped, owner, aad, []byte("later-committed"))
			require.NoError(t, err)
			require.NoError(t, pool.MerchantTx(scoped, owner, func(c context.Context, tx pgx.Tx) error {
				_, e := tx.Exec(c, `INSERT INTO openrails.merchant_configurations(merchant_id,config) VALUES($1,jsonb_build_object('ciphertext',$2::text))`, owner.UUID(), ciphertext)
				return e
			}))
			var persisted string
			require.NoError(t, pool.MerchantTx(scoped, owner, func(c context.Context, tx pgx.Tx) error {
				return tx.QueryRow(c, `SELECT config->>'ciphertext' FROM openrails.merchant_configurations WHERE merchant_id=$1`, owner.UUID()).Scan(&persisted)
			}))
			fresh, err := NewEncryptor(key, store)
			require.NoError(t, err)
			plaintext, err := fresh.Decrypt(scoped, owner, aad, persisted)
			require.NoError(t, err)
			require.Equal(t, "later-committed", string(plaintext))
			require.Equal(t, 1, countDEKs(t, ctx, pool, owner))
		})
	}
}

func TestDBDEKStore_RejectsMismatchedOrActiveLazyPin(t *testing.T) {
	pool, ctx := startCryptoPostgres(t)
	database, err := db.NewWithPGXPool(pool.Raw(), config.DefaultSchema)
	require.NoError(t, err)
	owner, other := seedMerchant(t, ctx, pool), seedMerchant(t, ctx, pool)
	scoped, release, err := database.WithMerchantConn(merchant.WithID(ctx, owner))
	require.NoError(t, err)
	defer release()
	store, err := NewDBDEKStore(pool)
	require.NoError(t, err)
	enc, err := NewEncryptor(masterKey(t), store)
	require.NoError(t, err)
	_, err = enc.Encrypt(scoped, other, testAAD(other), []byte("foreign scope"))
	require.Error(t, err)
	wrongSchema, err := NewDBDEKStore(db.WrapPool(pool.Raw(), "another_schema"))
	require.NoError(t, err)
	_, _, err = wrongSchema.GetWrappedDEK(scoped, owner)
	require.Error(t, err)
	otherPool, err := pgxpool.New(ctx, dbtest.SharedPostgresDSN(t))
	require.NoError(t, err)
	defer otherPool.Close()
	wrongPool, err := NewDBDEKStore(db.WrapPool(otherPool, config.DefaultSchema))
	require.NoError(t, err)
	_, _, err = wrongPool.GetWrappedDEK(scoped, owner)
	require.Error(t, err)
	// Even an active transaction reached through the known pin without a
	// callback marker must not be silently nested/committed by DEK custody.
	_, err = database.Qx(scoped).Exec(scoped, "BEGIN")
	require.NoError(t, err)
	_, err = enc.Encrypt(scoped, owner, testAAD(owner), []byte("active pin"))
	require.ErrorIs(t, err, db.ErrCallerTransaction)
	require.Empty(t, enc.dekGCMs)
	_, err = database.Qx(scoped).Exec(scoped, "ROLLBACK")
	require.NoError(t, err)
	_, err = enc.Encrypt(scoped, owner, testAAD(owner), []byte("idle pin"))
	require.NoError(t, err)
}
