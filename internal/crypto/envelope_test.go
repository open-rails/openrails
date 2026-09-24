package crypto

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/pkg/merchant"
)

// memDEKStore mirrors the DB store: the first wrapped DEK per merchant wins.
type memDEKStore struct {
	mu        sync.Mutex
	data      map[merchant.ID][]byte
	puts      int
	hideOnGet bool
	getErr    error
}

func (m *memDEKStore) GetWrappedDEK(_ context.Context, id merchant.ID) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.data[id]
	return w, ok && !m.hideOnGet, m.getErr
}

func (m *memDEKStore) PutWrappedDEK(_ context.Context, id merchant.ID, wrapped []byte) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.puts++
	if existing, ok := m.data[id]; ok {
		return existing, nil
	}
	m.data[id] = wrapped
	return wrapped, nil
}

func newStore() *memDEKStore { return &memDEKStore{data: map[merchant.ID][]byte{}} }

func masterKey(t *testing.T) string {
	key := make([]byte, keySize)
	_, err := rand.Read(key)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(key)
}

func newEncryptor(t *testing.T, master string, store DEKStore) *Encryptor {
	enc, err := NewEncryptor(master, store)
	require.NoError(t, err)
	require.True(t, enc.Enabled())
	return enc
}

func TestEnvelopeRoundTripAndTamperRefusal(t *testing.T) {
	ctx := context.Background()
	enc := newEncryptor(t, masterKey(t), newStore())
	a, b := merchant.ID(uuid.New()), merchant.ID(uuid.New())
	aad := SecretAAD(a, "psps/stripe/secret_key")
	for _, pt := range []string{"sk_live_super_secret", "", "\x00\xff\x10"} {
		ct, err := enc.Encrypt(ctx, a, aad, []byte(pt))
		require.NoError(t, err)
		again, _ := enc.Encrypt(ctx, a, aad, []byte(pt))
		require.NotEqual(t, ct, again, "random nonce per seal")
		got, err := enc.Decrypt(ctx, a, aad, ct)
		require.NoError(t, err)
		require.Equal(t, pt, string(got))
	}

	ct, err := enc.Encrypt(ctx, a, aad, []byte("secret"))
	require.NoError(t, err)
	raw, _ := base64.StdEncoding.DecodeString(ct)
	require.Len(t, raw, nonceSize+len("secret")+16)
	encode := base64.StdEncoding.EncodeToString
	type attempt struct {
		mid merchant.ID
		aad AAD
		ct  string
	}
	attempts := []attempt{
		{b, SecretAAD(b, "psps/stripe/secret_key"), ct}, // another merchant's DEK
		{a, SecretAAD(b, "psps/stripe/secret_key"), ct},
		{a, SecretAAD(a, "webhook_signing_secret"), ct}, // relocated to another row
		{a, nil, ct},
		{a, aad, "!!!"},
		{a, aad, encode(raw[:nonceSize-1])},
		{a, aad, encode(raw[:len(raw)-1])},
		{merchant.ID{}, aad, ct},
	}
	for _, i := range []int{0, nonceSize - 1, nonceSize, nonceSize + 2, len(raw) - 1} {
		tampered := append([]byte(nil), raw...)
		tampered[i] ^= 1
		attempts = append(attempts, attempt{a, aad, encode(tampered)})
	}
	for i, at := range attempts {
		pt, err := enc.Decrypt(ctx, at.mid, at.aad, at.ct)
		require.Error(t, err, "attempt %d", i)
		require.Nil(t, pt)
	}
	_, err = enc.Encrypt(ctx, merchant.ID{}, aad, []byte("x"))
	require.Error(t, err)

	// Length-prefixed AAD: no two (merchant, name) rows collide.
	seen := map[string]bool{}
	for _, x := range []AAD{SecretAAD(a, "ab"), SecretAAD(a, "a"), SecretAAD(a, "abc"), SecretAAD(a, ""), SecretAAD(b, "ab")} {
		require.False(t, seen[string(x)])
		seen[string(x)] = true
	}
}

func TestMerchantDEKLifecycle(t *testing.T) {
	ctx := context.Background()
	master, store := masterKey(t), newStore()
	a, b := merchant.ID(uuid.New()), merchant.ID(uuid.New())
	aad := SecretAAD(a, "k")

	enc := newEncryptor(t, master, store)
	require.Empty(t, store.data, "DEKs are created lazily")
	ct, err := enc.Encrypt(ctx, a, aad, []byte("persisted"))
	require.NoError(t, err)
	for range 3 {
		_, err = enc.Encrypt(ctx, a, aad, []byte("y"))
		require.NoError(t, err)
	}
	require.Equal(t, 1, store.puts, "one DEK per merchant, reused")

	got, err := newEncryptor(t, master, store).Decrypt(ctx, a, aad, ct)
	require.NoError(t, err)
	require.Equal(t, "persisted", string(got), "a restarted process unwraps the stored DEK")
	_, err = newEncryptor(t, masterKey(t), store).Decrypt(ctx, a, aad, ct)
	require.ErrorContains(t, err, "unwrap DEK", "wrong master key")

	store.data[b] = store.data[a]
	_, err = newEncryptor(t, master, store).Encrypt(ctx, b, SecretAAD(b, "k"), []byte("x"))
	require.ErrorContains(t, err, "unwrap DEK", "a wrapped DEK is bound to its merchant row")

	failing := newStore()
	failing.getErr = errors.New("db down")
	_, err = newEncryptor(t, master, failing).Encrypt(ctx, a, aad, []byte("x"))
	require.ErrorContains(t, err, "db down")

	// Concurrent first use: a loser that missed on read converges on the stored DEK.
	race := newStore()
	winner := newEncryptor(t, master, race)
	ct1, err := winner.Encrypt(ctx, a, aad, []byte("first"))
	require.NoError(t, err)
	race.hideOnGet = true
	loser := newEncryptor(t, master, race)
	ct2, err := loser.Encrypt(ctx, a, aad, []byte("second"))
	require.NoError(t, err)
	got, err = loser.Decrypt(ctx, a, aad, ct1)
	require.NoError(t, err)
	require.Equal(t, "first", string(got))
	got, err = winner.Decrypt(ctx, a, aad, ct2)
	require.NoError(t, err)
	require.Equal(t, "second", string(got))
}

func TestEncryptorConfiguration(t *testing.T) {
	disabled, err := NewEncryptor("", nil)
	require.NoError(t, err)
	require.False(t, disabled.Enabled())
	require.False(t, (*Encryptor)(nil).Enabled())
	m := merchant.ID(uuid.New())
	_, err = disabled.Encrypt(context.Background(), m, SecretAAD(m, "k"), []byte("x"))
	require.ErrorIs(t, err, ErrEncryptionDisabled)
	_, err = disabled.Decrypt(context.Background(), m, SecretAAD(m, "k"), "AAAA")
	require.ErrorIs(t, err, ErrEncryptionDisabled)
	for _, key := range []string{"not-base64!!!", base64.StdEncoding.EncodeToString([]byte("tooshort")), base64.StdEncoding.EncodeToString(make([]byte, keySize+1))} {
		_, err := NewEncryptor(key, newStore())
		require.Error(t, err, key)
	}
	_, err = NewEncryptor(masterKey(t), nil)
	require.ErrorContains(t, err, "DEK store is required")
}
