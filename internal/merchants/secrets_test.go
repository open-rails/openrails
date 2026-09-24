package merchants

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"path"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/crypto"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

const stripeKeyName = "psps/stripe/live/acct_884_test/secret_key"

type memDEKStore map[merchant.ID][]byte

func (m memDEKStore) GetWrappedDEK(_ context.Context, id merchant.ID) ([]byte, bool, error) {
	w, ok := m[id]
	return w, ok, nil
}

func (m memDEKStore) PutWrappedDEK(_ context.Context, id merchant.ID, wrapped []byte) ([]byte, error) {
	if existing, ok := m[id]; ok {
		return existing, nil
	}
	m[id] = wrapped
	return wrapped, nil
}

func newEncryptor(t *testing.T) *crypto.Encryptor {
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	enc, err := crypto.NewEncryptor(base64.StdEncoding.EncodeToString(key), memDEKStore{})
	require.NoError(t, err)
	return enc
}

// fakeVaultKV mimics KV-v2: versions increase on every write.
type fakeVaultKV struct {
	data    map[string]map[string]string
	version map[string]int
	err     error
}

func newFakeVaultKV() *fakeVaultKV {
	return &fakeVaultKV{data: map[string]map[string]string{}, version: map[string]int{}}
}

func (f *fakeVaultKV) ReadSecret(_ context.Context, p string) (map[string]string, int, error) {
	return f.data[p], f.version[p], f.err
}

func (f *fakeVaultKV) WriteSecret(_ context.Context, p string, data map[string]string) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.data[p] = data
	f.version[p]++
	return f.version[p], nil
}

func (f *fakeVaultKV) DeleteSecret(_ context.Context, p string) error {
	delete(f.data, p)
	return f.err
}

func (f *fakeVaultKV) ListSecrets(context.Context, string) ([]string, error) { return nil, f.err }

// TestSecretStoreContract runs every backend composition through the shared
// MerchantSecretStore contract.
func TestSecretStoreContract(t *testing.T) {
	for name, build := range map[string]func(t *testing.T) MerchantSecretStore{
		"memory": func(*testing.T) MerchantSecretStore { return NewMemorySecretStore() },
		"encrypted": func(t *testing.T) MerchantSecretStore {
			s, err := NewEncryptedSecretStore(NewMemorySecretStore(), newEncryptor(t))
			require.NoError(t, err)
			return s
		},
		"cached": func(*testing.T) MerchantSecretStore { return NewCachedSecretStore(NewMemorySecretStore(), time.Hour) },
		"vault":  func(*testing.T) MerchantSecretStore { return NewVaultSecretStore("secret", newFakeVaultKV()) },
		"guarded": func(*testing.T) MerchantSecretStore {
			return NewWriteRestrictedSecretStore(NewMemorySecretStore(), map[string]string{SolanaPrivateKeyWritePattern(): "no"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, store := t.Context(), build(t)
			a, b := merchant.ID(uuid.New()), merchant.ID(uuid.New())

			_, err := store.Get(ctx, a, stripeKeyName)
			require.ErrorIs(t, err, ErrSecretNotFound)
			require.NotErrorIs(t, err, ErrSecretBackendUnavailable, "absence is not an outage")

			v1, err := store.Put(ctx, a, stripeKeyName, "sk_a")
			require.NoError(t, err)
			require.Equal(t, "sk_a", v1.Value)
			_, err = store.Get(ctx, b, stripeKeyName)
			require.ErrorIs(t, err, ErrSecretNotFound, "merchants are namespaced")
			_, err = store.Put(ctx, b, stripeKeyName, "sk_b")
			require.NoError(t, err)
			got, err := store.Get(ctx, a, stripeKeyName)
			require.NoError(t, err)
			require.Equal(t, "sk_a", got.Value)

			v2, err := store.Put(ctx, a, stripeKeyName, "sk_a2")
			require.NoError(t, err)
			require.Greater(t, v2.Version, v1.Version, "rotation bumps the version")
			got, err = store.Get(ctx, a, stripeKeyName)
			require.NoError(t, err)
			require.Equal(t, Secret{Name: stripeKeyName, Value: "sk_a2", Version: v2.Version}, got)

			require.NoError(t, store.Delete(ctx, a, stripeKeyName))
			require.NoError(t, store.Delete(ctx, a, stripeKeyName), "delete is idempotent")
			_, err = store.Get(ctx, a, stripeKeyName)
			require.ErrorIs(t, err, ErrSecretNotFound)

			for _, bad := range []struct {
				id   merchant.ID
				name string
			}{{merchant.ID{}, stripeKeyName}, {a, ""}, {a, "psps/../../other/secret_key"}} {
				_, err := store.Get(ctx, bad.id, bad.name)
				require.Error(t, err)
				_, err = store.Put(ctx, bad.id, bad.name, "x")
				require.Error(t, err)
			}
		})
	}
}

func TestSamePlaintextPutDoesNotRotate(t *testing.T) {
	enc, err := NewEncryptedSecretStore(NewMemorySecretStore(), newEncryptor(t))
	require.NoError(t, err)
	// Ciphertext is randomized, so idempotency must compare plaintext.
	for name, store := range map[string]MerchantSecretStore{"memory": NewMemorySecretStore(), "encrypted": enc} {
		id := merchant.ID(uuid.New())
		v1, err := store.Put(t.Context(), id, stripeKeyName, "sk_1")
		require.NoError(t, err)
		again, err := store.Put(t.Context(), id, stripeKeyName, "sk_1")
		require.NoError(t, err)
		require.Equal(t, v1.Version, again.Version, name)
	}
}

func TestEncryptedSecretIsSealedToItsRow(t *testing.T) {
	ctx, inner, enc := t.Context(), NewMemorySecretStore(), newEncryptor(t)
	store, err := NewEncryptedSecretStore(inner, enc)
	require.NoError(t, err)
	a, b := merchant.ID(uuid.New()), merchant.ID(uuid.New())
	webhookName := "psps/stripe/live/acct_884_test/webhook_signing_secret"
	_, err = store.Put(ctx, a, webhookName, "whsec_real")
	require.NoError(t, err)
	blob, err := inner.Get(ctx, a, webhookName)
	require.NoError(t, err)
	require.NotContains(t, blob.Value, "whsec_real", "only ciphertext at rest")

	// SEC-24 item 1: a DB-write actor relocating ciphertext to another row of
	// the same merchant (same DEK) or another merchant must not get plaintext.
	_, err = inner.Put(ctx, a, stripeKeyName, blob.Value)
	require.NoError(t, err)
	_, err = store.Get(ctx, a, stripeKeyName)
	require.Error(t, err)
	_, err = inner.Put(ctx, b, webhookName, blob.Value)
	require.NoError(t, err)
	_, err = store.Get(ctx, b, webhookName)
	require.Error(t, err)

	got, err := store.Get(ctx, a, webhookName)
	require.NoError(t, err)
	require.Equal(t, "whsec_real", got.Value)

	passthrough, err := NewEncryptedSecretStore(inner, nil)
	require.NoError(t, err)
	require.Same(t, inner, passthrough)
	_, err = NewEncryptedSecretStore(nil, enc)
	require.Error(t, err)
}

// countingStore observes cache read-through and can fail on demand.
type countingStore struct {
	MerchantSecretStore
	gets   int
	getErr error
}

func (s *countingStore) Get(ctx context.Context, id merchant.ID, name string) (Secret, error) {
	s.gets++
	if s.getErr != nil {
		return Secret{}, s.getErr
	}
	return s.MerchantSecretStore.Get(ctx, id, name)
}

func TestCachedSecretStore(t *testing.T) {
	ctx, id := t.Context(), merchant.ID(uuid.New())
	backend := &countingStore{MerchantSecretStore: NewMemorySecretStore()}
	cache := NewCachedSecretStore(backend, 45*time.Second).(*cachedSecretStore)
	now := time.Unix(1_700_000_000, 0)
	cache.now = func() time.Time { return now }

	_, err := backend.Put(ctx, id, stripeKeyName, "sk_1")
	require.NoError(t, err)
	for range 3 {
		got, err := cache.Get(ctx, id, stripeKeyName)
		require.NoError(t, err)
		require.Equal(t, "sk_1", got.Value)
	}
	require.Equal(t, 1, backend.gets, "served from cache within TTL")
	now = now.Add(46 * time.Second)
	_, err = cache.Get(ctx, id, stripeKeyName)
	require.NoError(t, err)
	require.Equal(t, 2, backend.gets, "expired entry reads through")

	_, err = cache.Put(ctx, id, stripeKeyName, "sk_2")
	require.NoError(t, err)
	got, err := cache.Get(ctx, id, stripeKeyName)
	require.NoError(t, err)
	require.Equal(t, "sk_2", got.Value)
	require.Equal(t, 2, backend.gets, "write-through refresh, no backend read")

	require.NoError(t, cache.Delete(ctx, id, stripeKeyName))
	_, err = cache.Get(ctx, id, stripeKeyName)
	require.ErrorIs(t, err, ErrSecretNotFound, "delete invalidates")

	backend.getErr = ErrSecretBackendUnavailable
	_, err = cache.Get(ctx, id, stripeKeyName)
	require.ErrorIs(t, err, ErrSecretBackendUnavailable)
	backend.getErr = nil
	_, err = backend.Put(ctx, id, stripeKeyName, "sk_ok")
	require.NoError(t, err)
	got, err = cache.Get(ctx, id, stripeKeyName)
	require.NoError(t, err, "errors are not cached")
	require.Equal(t, "sk_ok", got.Value)

	inner := NewMemorySecretStore()
	require.Same(t, inner, NewCachedSecretStore(inner, 0))
	require.Same(t, inner, NewCachedSecretStore(inner, -time.Second))
}

// or#812: a recorded rotation floor is never satisfied by a lagging backend
// or a pre-rotation cache entry, and recovers once the backend catches up.
func TestCredentialFloorRefusesBackendLagAndRecovers(t *testing.T) {
	for _, cached := range []bool{false, true} {
		ctx, owner := t.Context(), merchant.ID(uuid.New())
		backend := NewMemorySecretStore()
		first, err := backend.Put(ctx, owner, stripeKeyName, "first")
		require.NoError(t, err)
		reader := backend
		if cached {
			reader = NewCachedSecretStore(backend, time.Hour)
		}
		_, err = reader.Get(ctx, owner, first.Name) // warm any cache
		require.NoError(t, err)

		ref := SecretRef{Name: first.Name, MinVersion: first.Version + 1}
		value, err := ReadSecretRef(ctx, reader, owner, ref)
		require.ErrorIs(t, err, ErrSecretBackendUnavailable, "cached=%v", cached)
		require.Empty(t, value.Value)
		if versioned, ok := reader.(VersionedSecretReader); ok {
			_, err = versioned.GetAtLeastVersion(ctx, owner, ref.Name, ref.MinVersion)
			require.ErrorIs(t, err, ErrSecretBackendUnavailable)
		}

		second, err := backend.Put(ctx, owner, first.Name, "second")
		require.NoError(t, err)
		value, err = ReadSecretRef(ctx, reader, owner, ref)
		require.NoError(t, err)
		require.Equal(t, second, value)
	}
	_, err := ReadSecretRef(t.Context(), NewMemorySecretStore(), merchant.ID(uuid.New()), SecretRef{Name: stripeKeyName, Retired: true})
	require.ErrorIs(t, err, ErrSecretNotFound, "a retired credential is never read")
	_, err = ReadSecretRef(t.Context(), NewMemorySecretStore(), merchant.ID(uuid.New()), SecretRef{Name: stripeKeyName, MinVersion: -1})
	require.ErrorIs(t, err, ErrSecretBackendUnavailable)
}

func TestVaultSecretStore(t *testing.T) {
	ctx, id := t.Context(), merchant.ID(uuid.New())
	kv := newFakeVaultKV()
	store := NewVaultSecretStore("secret", kv)
	_, err := store.Put(ctx, id, stripeKeyName, "sk_live")
	require.NoError(t, err)
	require.Contains(t, kv.data, path.Join("secret/openrails/merchants", id.String(), stripeKeyName), "merchant-scoped path")
	require.Len(t, kv.data, 1)

	// An unreachable Vault is retryable, never "secret absent".
	kv.err = errors.New("dial tcp: connection refused")
	_, err = store.Get(ctx, id, stripeKeyName)
	require.ErrorIs(t, err, ErrSecretBackendUnavailable)
	require.NotErrorIs(t, err, ErrSecretNotFound)

	// An unwired Vault fails closed as unavailable.
	require.ErrorIs(t, ErrVaultNotConfigured, ErrSecretBackendUnavailable)
	stub := NewVaultSecretStore("secret", nil)
	_, err = stub.Get(ctx, id, stripeKeyName)
	require.ErrorIs(t, err, ErrVaultNotConfigured)
	_, err = stub.Put(ctx, id, stripeKeyName, "x")
	require.ErrorIs(t, err, ErrVaultNotConfigured)

	for _, prefix := range []string{"../escape", "a//b", "/abs", "a/./b"} {
		_, err := NewVaultSecretStoreWithPrefix("secret", prefix, kv)
		require.Error(t, err, prefix)
	}
}

func TestWriteRestrictedSecretStore(t *testing.T) {
	ctx, id := t.Context(), merchant.ID(uuid.New())
	solanaKey, err := PSPSecretName("solana", "live", "AKnL4NNf3DGWZJS6cPknBuEGnVsV4A4m5tgebLHaRSZ9", "private_key")
	require.NoError(t, err)
	inner := NewMemorySecretStore()
	_, err = inner.Put(ctx, id, solanaKey, "existing")
	require.NoError(t, err)
	store := NewWriteRestrictedSecretStore(inner, map[string]string{SolanaPrivateKeyWritePattern(): "encryption required"})

	_, err = store.Put(ctx, id, solanaKey, "new")
	require.ErrorContains(t, err, "encryption required")
	got, err := store.Get(ctx, id, solanaKey)
	require.NoError(t, err)
	require.Equal(t, "existing", got.Value, "existing restricted secrets stay readable")
	_, err = store.Put(ctx, id, stripeKeyName, "sk_test_123")
	require.NoError(t, err)

	// The pattern is derived from the canonical builder (SEC-20), so a rename
	// of the name shape cannot silently disarm the guard.
	for _, env := range []string{"live", "test"} {
		name, err := PSPSecretName("solana", env, "acct", "private_key")
		require.NoError(t, err)
		matched, err := path.Match(SolanaPrivateKeyWritePattern(), name)
		require.NoError(t, err)
		require.True(t, matched, name)
	}
	matched, _ := path.Match(SolanaPrivateKeyWritePattern(), stripeKeyName)
	require.False(t, matched)
}
