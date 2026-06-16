package crypto

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/pkg/merchant"
)

// memDEKStore is an in-memory DEKStore for unit tests. PutWrappedDEK keeps the
// first wrapped DEK ever stored for a tenant (mirroring the DB store's
// concurrent-first-use semantics) and returns it.
type memDEKStore struct {
	mu   sync.Mutex
	data map[string][]byte
	puts int
	gets int
}

func newMemDEKStore() *memDEKStore { return &memDEKStore{data: map[string][]byte{}} }

func (m *memDEKStore) GetWrappedDEK(_ context.Context, id merchant.ID) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gets++
	w, ok := m.data[id.String()]
	return w, ok, nil
}

func (m *memDEKStore) PutWrappedDEK(_ context.Context, id merchant.ID, wrapped []byte) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.puts++
	if existing, ok := m.data[id.String()]; ok {
		return existing, nil
	}
	m.data[id.String()] = wrapped
	return wrapped, nil
}

func testMasterKey(t *testing.T) string {
	t.Helper()
	key := make([]byte, keySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return base64.StdEncoding.EncodeToString(key)
}

func newTenant(t *testing.T) merchant.ID {
	t.Helper()
	return merchant.ID(uuid.New())
}

func TestEncrypt_RoundTripsPerTenant(t *testing.T) {
	ctx := context.Background()
	enc, err := NewEncryptor(testMasterKey(t), newMemDEKStore())
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	if !enc.Enabled() {
		t.Fatal("encryptor should be enabled with a master key")
	}

	tA := newTenant(t)
	plaintext := []byte("sk_live_super_secret")
	ct, err := enc.Encrypt(ctx, tA, plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if ct == string(plaintext) {
		t.Fatal("ciphertext must not equal plaintext")
	}
	got, err := enc.Decrypt(ctx, tA, ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(got) != string(plaintext) {
		t.Fatalf("round trip mismatch: got %q want %q", got, plaintext)
	}
}

func TestEncrypt_NonDeterministic(t *testing.T) {
	ctx := context.Background()
	enc, _ := NewEncryptor(testMasterKey(t), newMemDEKStore())
	tA := newTenant(t)
	c1, _ := enc.Encrypt(ctx, tA, []byte("same"))
	c2, _ := enc.Encrypt(ctx, tA, []byte("same"))
	if c1 == c2 {
		t.Fatal("two encryptions of the same plaintext must differ (random nonce)")
	}
}

func TestDecrypt_CrossTenantFails(t *testing.T) {
	ctx := context.Background()
	enc, _ := NewEncryptor(testMasterKey(t), newMemDEKStore())
	tA := newTenant(t)
	tB := newTenant(t)

	ctA, err := enc.Encrypt(ctx, tA, []byte("tenant-A-secret"))
	if err != nil {
		t.Fatalf("Encrypt A: %v", err)
	}
	// Merchant B's DEK must NOT be able to decrypt tenant A's ciphertext.
	if _, err := enc.Decrypt(ctx, tB, ctA); err == nil {
		t.Fatal("tenant B must not decrypt tenant A's ciphertext")
	}
}

func TestDEK_CreatedLazilyAndReused(t *testing.T) {
	ctx := context.Background()
	store := newMemDEKStore()
	enc, _ := NewEncryptor(testMasterKey(t), store)
	tA := newTenant(t)

	// No DEK exists before first use.
	if _, ok, _ := store.GetWrappedDEK(ctx, tA); ok {
		t.Fatal("DEK should not exist before first use")
	}

	// First encrypt creates+stores exactly one wrapped DEK.
	if _, err := enc.Encrypt(ctx, tA, []byte("x")); err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if store.puts != 1 {
		t.Fatalf("expected exactly 1 DEK put, got %d", store.puts)
	}
	if _, ok, _ := store.GetWrappedDEK(ctx, tA); !ok {
		t.Fatal("wrapped DEK should be stored after first use")
	}

	// Subsequent ops reuse the cached DEK (no further puts).
	for i := 0; i < 3; i++ {
		if _, err := enc.Encrypt(ctx, tA, []byte("y")); err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
	}
	if store.puts != 1 {
		t.Fatalf("DEK must be reused, not recreated; got %d puts", store.puts)
	}
}

func TestDEK_SurvivesNewEncryptorInstance(t *testing.T) {
	ctx := context.Background()
	master := testMasterKey(t)
	store := newMemDEKStore()
	tA := newTenant(t)

	enc1, _ := NewEncryptor(master, store)
	ct, err := enc1.Encrypt(ctx, tA, []byte("persisted"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// A fresh encryptor (cold cache) over the same store + master key must unwrap
	// the stored DEK and decrypt successfully.
	enc2, _ := NewEncryptor(master, store)
	got, err := enc2.Decrypt(ctx, tA, ct)
	if err != nil {
		t.Fatalf("Decrypt with fresh encryptor: %v", err)
	}
	if string(got) != "persisted" {
		t.Fatalf("got %q", got)
	}
}

func TestDecrypt_WrongMasterKeyFails(t *testing.T) {
	ctx := context.Background()
	store := newMemDEKStore()
	tA := newTenant(t)

	enc1, _ := NewEncryptor(testMasterKey(t), store)
	ct, _ := enc1.Encrypt(ctx, tA, []byte("secret"))

	// A different master key cannot unwrap the stored DEK.
	enc2, _ := NewEncryptor(testMasterKey(t), store)
	if _, err := enc2.Decrypt(ctx, tA, ct); err == nil {
		t.Fatal("decrypt with wrong master key must fail")
	}
}

func TestDisabledEncryptor(t *testing.T) {
	ctx := context.Background()
	enc, err := NewEncryptor("", nil)
	if err != nil {
		t.Fatalf("NewEncryptor disabled: %v", err)
	}
	if enc.Enabled() {
		t.Fatal("encryptor with no master key must be disabled")
	}
	if _, err := enc.Encrypt(ctx, newTenant(t), []byte("x")); !errors.Is(err, ErrEncryptionDisabled) {
		t.Fatalf("expected ErrEncryptionDisabled, got %v", err)
	}
}

func TestNewEncryptor_RejectsBadMasterKey(t *testing.T) {
	if _, err := NewEncryptor("not-base64!!!", newMemDEKStore()); err == nil {
		t.Fatal("expected error for non-base64 master key")
	}
	short := base64.StdEncoding.EncodeToString([]byte("tooshort"))
	if _, err := NewEncryptor(short, newMemDEKStore()); err == nil {
		t.Fatal("expected error for wrong-length master key")
	}
}
