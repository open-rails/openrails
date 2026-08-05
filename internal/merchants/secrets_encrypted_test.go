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

	put, err := store.Put(ctx, tA, "psps/stripe/live/acct_884_test/secret_key", "sk_live_abc")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if put.Value != "sk_live_abc" {
		t.Fatalf("Put returned %q, want plaintext echoed back", put.Value)
	}

	// The INNER store must hold ciphertext, never the plaintext.
	raw, err := inner.Get(ctx, tA, "psps/stripe/live/acct_884_test/secret_key")
	if err != nil {
		t.Fatalf("inner Get: %v", err)
	}
	if raw.Value == "sk_live_abc" {
		t.Fatal("inner store must hold ciphertext, not plaintext")
	}

	got, err := store.Get(ctx, tA, "psps/stripe/live/acct_884_test/secret_key")
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

	v1, _ := store.Put(ctx, tA, "psps/stripe/live/acct_884_test/secret_key", "sk_1")
	v1again, _ := store.Put(ctx, tA, "psps/stripe/live/acct_884_test/secret_key", "sk_1")
	if v1again.Version != v1.Version {
		t.Fatalf("re-putting the same plaintext must not bump version (%d -> %d)", v1.Version, v1again.Version)
	}
	v2, _ := store.Put(ctx, tA, "psps/stripe/live/acct_884_test/secret_key", "sk_2")
	if v2.Version != v1.Version+1 {
		t.Fatalf("changing the value must bump version, got %d want %d", v2.Version, v1.Version+1)
	}
}

func TestEncryptedSecretStore_CrossMerchantIsolation(t *testing.T) {
	ctx := context.Background()
	inner := NewMemorySecretStore()
	enc := newEnc(t)
	store, _ := NewEncryptedSecretStore(inner, enc)
	tA := merchant.ID(uuid.New())
	tB := merchant.ID(uuid.New())

	if _, err := store.Put(ctx, tA, "psps/stripe/live/acct_884_test/secret_key", "A-secret"); err != nil {
		t.Fatalf("Put A: %v", err)
	}
	// Merchant B has its own DEK; merchant A's ciphertext is unreadable as B.
	rawA, _ := inner.Get(ctx, tA, "psps/stripe/live/acct_884_test/secret_key")
	if _, err := enc.Decrypt(ctx, tB, crypto.SecretAAD(tB, "psps/stripe/live/acct_884_test/secret_key"), rawA.Value); err == nil {
		t.Fatal("merchant B must not decrypt merchant A's stored secret")
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

// TestEncryptedSecretStore_CiphertextIsBoundToItsRow is the SEC-24 item 1
// guard. Distinct DEKs already stop CROSS-MERCHANT relocation. This is the
// within-merchant case the audit named: an actor with DB WRITE access moves one
// merchant's webhook_signing_secret blob into its security_key row, or an NMI
// test key into the Stripe live-key slot. Same DEK, so before AAD binding it
// decrypted cleanly and the process then used the wrong credential.
func TestEncryptedSecretStore_CiphertextIsBoundToItsRow(t *testing.T) {
	ctx := context.Background()
	inner := NewMemorySecretStore()
	enc := newEnc(t)
	store, _ := NewEncryptedSecretStore(inner, enc)
	tA := merchant.ID(uuid.New())

	if _, err := store.Put(ctx, tA, "psps/stripe/live/acct_884_test/webhook_signing_secret", "whsec_real"); err != nil {
		t.Fatalf("Put webhook secret: %v", err)
	}
	blob, err := inner.Get(ctx, tA, "psps/stripe/live/acct_884_test/webhook_signing_secret")
	if err != nil {
		t.Fatalf("read raw ciphertext: %v", err)
	}

	// Relocate the ciphertext into a DIFFERENT name for the SAME merchant —
	// exactly what a DB-write actor can do.
	if _, err := inner.Put(ctx, tA, "psps/stripe/live/acct_884_test/secret_key", blob.Value); err != nil {
		t.Fatalf("relocate blob: %v", err)
	}
	if _, err := store.Get(ctx, tA, "psps/stripe/live/acct_884_test/secret_key"); err == nil {
		t.Fatal("a ciphertext moved into another (merchant, name) row must NOT decrypt: " +
			"AAD binds it to the row it was sealed for (SEC-24 item 1)")
	}

	// The row it belongs to still round-trips.
	got, err := store.Get(ctx, tA, "psps/stripe/live/acct_884_test/webhook_signing_secret")
	if err != nil {
		t.Fatalf("original row must still decrypt: %v", err)
	}
	if got.Value != "whsec_real" {
		t.Fatalf("round-trip mismatch: %q", got.Value)
	}
}
