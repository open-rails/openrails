package solana

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/open-rails/openrails/pkg/tenant"
)

// fakeSecrets is an in-memory TenantSecretGetter that counts reads so tests can
// assert caching behavior.
type fakeSecrets struct {
	mu    sync.Mutex
	value string
	err   error
	reads int
}

func (f *fakeSecrets) GetSecret(_ context.Context, _ tenant.ID, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.err != nil {
		return "", f.err
	}
	return f.value, nil
}

func (f *fakeSecrets) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func newTestKey(t *testing.T) solanago.PrivateKey {
	t.Helper()
	key, err := solanago.NewRandomPrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

func TestKeypairSignerPublicKeyAndSign(t *testing.T) {
	key := newTestKey(t)
	secrets := &fakeSecrets{value: key.String()}
	signer := NewKeypairSigner(secrets, time.Minute)
	ctx := context.Background()
	tid := tenant.DefaultID

	pub, err := signer.PublicKey(ctx, tid)
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}
	if !pub.Equals(key.PublicKey()) {
		t.Fatalf("PublicKey = %s, want %s", pub, key.PublicKey())
	}

	msg := []byte("solana subscription transfer message")
	sig, err := signer.SignMessage(ctx, tid, msg)
	if err != nil {
		t.Fatalf("SignMessage: %v", err)
	}
	if !sig.Verify(pub, msg) {
		t.Fatal("signature did not verify against the tenant public key")
	}
}

func TestKeypairSignerCachesWithinTTL(t *testing.T) {
	key := newTestKey(t)
	secrets := &fakeSecrets{value: key.String()}
	signer := NewKeypairSigner(secrets, time.Minute)
	ctx := context.Background()
	tid := tenant.DefaultID

	for i := range 5 {
		if _, err := signer.PublicKey(ctx, tid); err != nil {
			t.Fatalf("PublicKey #%d: %v", i, err)
		}
	}
	if got := secrets.readCount(); got != 1 {
		t.Fatalf("secret reads = %d, want 1 (cached within ttl)", got)
	}
}

func TestKeypairSignerRefetchesAfterTTL(t *testing.T) {
	key := newTestKey(t)
	secrets := &fakeSecrets{value: key.String()}
	ks := &keypairSigner{secrets: secrets, ttl: time.Minute, cache: make(map[string]cachedKey)}

	current := time.Unix(1_700_000_000, 0)
	ks.now = func() time.Time { return current }
	ctx := context.Background()
	tid := tenant.DefaultID

	if _, err := ks.PublicKey(ctx, tid); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Within ttl: cached.
	current = current.Add(30 * time.Second)
	if _, err := ks.PublicKey(ctx, tid); err != nil {
		t.Fatalf("second: %v", err)
	}
	// Past ttl: refetch.
	current = current.Add(time.Minute)
	if _, err := ks.PublicKey(ctx, tid); err != nil {
		t.Fatalf("third: %v", err)
	}
	if got := secrets.readCount(); got != 2 {
		t.Fatalf("secret reads = %d, want 2 (one per ttl window)", got)
	}
}

func TestKeypairSignerFailClosed(t *testing.T) {
	secrets := &fakeSecrets{err: errors.New("vault unreachable")}
	signer := NewKeypairSigner(secrets, time.Minute)
	if _, err := signer.PublicKey(context.Background(), tenant.DefaultID); err == nil {
		t.Fatal("expected error when secret store fails, got nil")
	}
}

func TestKeypairSignerRejectsZeroTenant(t *testing.T) {
	key := newTestKey(t)
	signer := NewKeypairSigner(&fakeSecrets{value: key.String()}, time.Minute)
	if _, err := signer.SignMessage(context.Background(), tenant.ID{}, []byte("x")); err == nil {
		t.Fatal("expected error for zero tenant id, got nil")
	}
}
