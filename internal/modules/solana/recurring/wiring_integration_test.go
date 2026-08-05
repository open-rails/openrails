//go:build integration

package recurring

import (
	"context"
	"sync"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestMain(m *testing.M) { dbtest.RunMain(m) }

type signerTestSecrets struct{}

func (signerTestSecrets) Get(context.Context, merchant.ID, string) (merchants.Secret, error) {
	return merchants.Secret{}, merchants.ErrSecretNotFound
}

type signerTestSecretMap map[string]string

func (s signerTestSecretMap) Get(_ context.Context, _ merchant.ID, name string) (merchants.Secret, error) {
	value, ok := s[name]
	if !ok {
		return merchants.Secret{}, merchants.ErrSecretNotFound
	}
	return merchants.Secret{Name: name, Value: value}, nil
}

type signerTestTransit struct {
	mu    sync.Mutex
	key   solanago.PrivateKey
	names []string
}

func (f *signerTestTransit) Sign(_ context.Context, name string, input []byte) ([]byte, error) {
	f.mu.Lock()
	f.names = append(f.names, name)
	f.mu.Unlock()
	sig, err := f.key.Sign(input)
	if err != nil {
		return nil, err
	}
	return sig[:], nil
}

func (f *signerTestTransit) PublicKey(_ context.Context, name string) ([]byte, error) {
	f.mu.Lock()
	f.names = append(f.names, name)
	f.mu.Unlock()
	pub := f.key.PublicKey()
	return pub[:], nil
}

func TestPSPSignerUsesVaultTransitEvidence(t *testing.T) {
	ctx := context.Background()
	_, appDSN := dbtest.SharedRLSPostgres(t)
	pool, err := pgxpool.New(ctx, appDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	appDB, err := db.NewWithPGXPool(pool, "")
	require.NoError(t, err)

	merchantUUID := uuid.New()
	tid := merchant.ID(merchantUUID)
	_, err = pool.Exec(ctx,
		`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`,
		merchantUUID, "solana-signer-"+merchantUUID.String()[:8])
	require.NoError(t, err)

	key, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	const vaultKey = "openrails-solana-it"
	now := time.Now().UTC()
	require.NoError(t, appDB.RunInMerchantConn(merchant.WithID(ctx, tid), func(ctx context.Context) error {
		_, err := appDB.Gen(ctx).UpsertPSP(ctx, gen.UpsertPSPParams{
			MerchantID:     merchantUUID,
			Rail:           "solana",
			AccountID:      key.PublicKey().String(),
			Evidence:       []byte(`{"signer":{"mode":"vault_transit","key":"` + vaultKey + `"}}`),
			LastVerifiedAt: &now,
		})
		return err
	}))

	transit := &signerTestTransit{key: key}
	signer := NewSignerFromPSPs(signerTestSecrets{}, transit, appDB, 0, "live")
	pub, err := signer.PublicKey(ctx, tid)
	require.NoError(t, err)
	require.True(t, pub.Equals(key.PublicKey()))

	msg := []byte("solana PSP signer message")
	sig, err := signer.SignMessage(ctx, tid, msg)
	require.NoError(t, err)
	require.True(t, sig.Verify(pub, msg))
	require.Equal(t, []string{vaultKey, vaultKey}, transit.names)
}

func TestPSPSignerUsesConfiguredEnvironment(t *testing.T) {
	ctx := context.Background()
	_, appDSN := dbtest.SharedRLSPostgres(t)
	pool, err := pgxpool.New(ctx, appDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	appDB, err := db.NewWithPGXPool(pool, "")
	require.NoError(t, err)

	merchantUUID := uuid.New()
	tid := merchant.ID(merchantUUID)
	_, err = pool.Exec(ctx,
		`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`,
		merchantUUID, "solana-signer-env-"+merchantUUID.String()[:8])
	require.NoError(t, err)

	key, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	environment := "test"
	now := time.Now().UTC()
	require.NoError(t, appDB.RunInMerchantConn(merchant.WithID(ctx, tid), func(ctx context.Context) error {
		_, err := appDB.Gen(ctx).UpsertPSP(ctx, gen.UpsertPSPParams{
			MerchantID:     merchantUUID,
			Rail:           "solana",
			Environment:    &environment,
			AccountID:      key.PublicKey().String(),
			Evidence:       []byte(`{"signer":{"mode":"local_keypair"}}`),
			LastVerifiedAt: &now,
		})
		return err
	}))
	secretName, err := merchants.PSPSecretName("solana", environment, key.PublicKey().String(), "private_key")
	require.NoError(t, err)

	signer := NewSignerFromPSPs(signerTestSecretMap{secretName: key.String()}, nil, appDB, 0, environment)
	pub, err := signer.PublicKey(ctx, tid)
	require.NoError(t, err)
	require.True(t, pub.Equals(key.PublicKey()))
}

func TestPSPSignerSignsForRecordedPublicKey(t *testing.T) {
	ctx := context.Background()
	_, appDSN := dbtest.SharedRLSPostgres(t)
	pool, err := pgxpool.New(ctx, appDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	appDB, err := db.NewWithPGXPool(pool, "")
	require.NoError(t, err)

	merchantUUID := uuid.New()
	tid := merchant.ID(merchantUUID)
	_, err = pool.Exec(ctx,
		`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`,
		merchantUUID, "solana-signer-recorded-"+merchantUUID.String()[:8])
	require.NoError(t, err)

	environment := "test"
	oldKey, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	newKey, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, appDB.RunInMerchantConn(merchant.WithID(ctx, tid), func(ctx context.Context) error {
		for _, key := range []solanago.PrivateKey{oldKey, newKey} {
			if _, err := appDB.Gen(ctx).UpsertPSP(ctx, gen.UpsertPSPParams{
				MerchantID:     merchantUUID,
				Rail:           "solana",
				Environment:    &environment,
				AccountID:      key.PublicKey().String(),
				Evidence:       []byte(`{"signer":{"mode":"local_keypair"}}`),
				LastVerifiedAt: &now,
			}); err != nil {
				return err
			}
		}
		return nil
	}))
	oldSecret, err := merchants.PSPSecretName("solana", environment, oldKey.PublicKey().String(), "private_key")
	require.NoError(t, err)
	newSecret, err := merchants.PSPSecretName("solana", environment, newKey.PublicKey().String(), "private_key")
	require.NoError(t, err)

	signer := NewSignerFromPSPs(signerTestSecretMap{
		oldSecret: oldKey.String(),
		newSecret: newKey.String(),
	}, nil, appDB, 0, environment)
	addressSigner, ok := signer.(interface {
		SignMessageForPublicKey(context.Context, merchant.ID, solanago.PublicKey, []byte) (solanago.Signature, error)
	})
	require.True(t, ok)

	msg := []byte("sign for recorded subscription authority")
	sig, err := addressSigner.SignMessageForPublicKey(ctx, tid, oldKey.PublicKey(), msg)
	require.NoError(t, err)
	require.True(t, sig.Verify(oldKey.PublicKey(), msg))
	require.False(t, sig.Verify(newKey.PublicKey(), msg))
}
