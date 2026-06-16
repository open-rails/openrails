package merchants

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"testing"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/crypto"
	"github.com/open-rails/openrails/pkg/merchant"
)

// memDEKStore is an in-memory DEKStore for the encrypted-store unit tests.
type memDEKStore struct{ data map[string][]byte }

func (m *memDEKStore) GetWrappedDEK(_ context.Context, id merchant.ID) ([]byte, bool, error) {
	w, ok := m.data[id.String()]
	return w, ok, nil
}

func (m *memDEKStore) PutWrappedDEK(_ context.Context, id merchant.ID, wrapped []byte) ([]byte, error) {
	if existing, ok := m.data[id.String()]; ok {
		return existing, nil
	}
	m.data[id.String()] = wrapped
	return wrapped, nil
}

func newEnc(t *testing.T) *crypto.Encryptor {
	t.Helper()
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	enc, err := crypto.NewEncryptor(base64.StdEncoding.EncodeToString(key), &memDEKStore{data: map[string][]byte{}})
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	return enc
}

func TestEncryptedSecretStore_RoundTrips(t *testing.T) {
	ctx := context.Background()
	inner := NewMemorySecretStore()
	store, err := NewEncryptedSecretStore(inner, newEnc(t))
	if err != nil {
		t.Fatalf("NewEncryptedSecretStore: %v", err)
	}
	tA := merchant.ID(uuid.New())

	put, err := store.Put(ctx, tA, SecretStripeSecretKey, "sk_live_abc")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if put.Value != "sk_live_abc" {
		t.Fatalf("Put returned %q, want plaintext echoed back", put.Value)
	}

	// The INNER store must hold ciphertext, never the plaintext.
	raw, err := inner.Get(ctx, tA, SecretStripeSecretKey)
	if err != nil {
		t.Fatalf("inner Get: %v", err)
	}
	if raw.Value == "sk_live_abc" {
		t.Fatal("inner store must hold ciphertext, not plaintext")
	}

	got, err := store.Get(ctx, tA, SecretStripeSecretKey)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Value != "sk_live_abc" {
		t.Fatalf("Get returned %q, want decrypted plaintext", got.Value)
	}
}

func TestEncryptedSecretStore_IdempotentRotation(t *testing.T) {
	ctx := context.Background()
	store, _ := NewEncryptedSecretStore(NewMemorySecretStore(), newEnc(t))
	tA := merchant.ID(uuid.New())

	v1, _ := store.Put(ctx, tA, SecretStripeSecretKey, "sk_1")
	v1again, _ := store.Put(ctx, tA, SecretStripeSecretKey, "sk_1")
	if v1again.Version != v1.Version {
		t.Fatalf("re-putting the same plaintext must not bump version (%d -> %d)", v1.Version, v1again.Version)
	}
	v2, _ := store.Put(ctx, tA, SecretStripeSecretKey, "sk_2")
	if v2.Version != v1.Version+1 {
		t.Fatalf("changing the value must bump version, got %d want %d", v2.Version, v1.Version+1)
	}
}

func TestEncryptedSecretStore_CrossTenantIsolation(t *testing.T) {
	ctx := context.Background()
	inner := NewMemorySecretStore()
	enc := newEnc(t)
	store, _ := NewEncryptedSecretStore(inner, enc)
	tA := merchant.ID(uuid.New())
	tB := merchant.ID(uuid.New())

	if _, err := store.Put(ctx, tA, SecretStripeSecretKey, "A-secret"); err != nil {
		t.Fatalf("Put A: %v", err)
	}
	// Merchant B has its own DEK; tenant A's ciphertext is unreadable as B.
	rawA, _ := inner.Get(ctx, tA, SecretStripeSecretKey)
	if _, err := enc.Decrypt(ctx, tB, rawA.Value); err == nil {
		t.Fatal("tenant B must not decrypt tenant A's stored secret")
	}
}

func TestNewEncryptedSecretStore_DisabledPassThrough(t *testing.T) {
	inner := NewMemorySecretStore()
	// nil / disabled encryptor returns the inner store unchanged.
	got, err := NewEncryptedSecretStore(inner, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != inner {
		t.Fatal("disabled encryptor must return the inner store unchanged")
	}
}
