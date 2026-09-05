package merchants

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// countingStore is a MerchantSecretStore that records how many times each method
// hits the "backend", and can be told to fail, so cache behaviour is observable.
type countingStore struct {
	mu      sync.Mutex
	values  map[string]Secret
	gets    int
	puts    int
	deletes int
	getErr  error // when set, Get returns this instead of a value
}

func newCountingStore() *countingStore {
	return &countingStore{values: map[string]Secret{}}
}

func (s *countingStore) key(id merchant.ID, name string) string { return id.String() + "|" + name }

func (s *countingStore) Get(_ context.Context, id merchant.ID, name string) (Secret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	if s.getErr != nil {
		return Secret{}, s.getErr
	}
	v, ok := s.values[s.key(id, name)]
	if !ok {
		return Secret{}, ErrSecretNotFound
	}
	return v, nil
}

func (s *countingStore) Put(_ context.Context, id merchant.ID, name, value string) (Secret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts++
	prev := s.values[s.key(id, name)]
	sec := Secret{Name: name, Value: value, Version: prev.Version + 1}
	s.values[s.key(id, name)] = sec
	return sec, nil
}

func (s *countingStore) Delete(_ context.Context, id merchant.ID, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletes++
	delete(s.values, s.key(id, name))
	return nil
}

func (s *countingStore) List(_ context.Context, _ merchant.ID) ([]string, error) { return nil, nil }

// withClock returns the wrapped cache plus a function to advance its clock.
func withClock(t *testing.T, inner MerchantSecretStore, ttl time.Duration) (MerchantSecretStore, func(time.Duration)) {
	t.Helper()
	store := NewCachedSecretStore(inner, ttl)
	c, ok := store.(*cachedSecretStore)
	if !ok {
		t.Fatalf("expected *cachedSecretStore, got %T", store)
	}
	var mu sync.Mutex
	now := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	return store, func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
}

func TestCachedSecretStore_TTLHitThenExpiry(t *testing.T) {
	ctx := context.Background()
	backend := newCountingStore()
	id := dbtest.TestMerchantID
	if _, err := backend.Put(ctx, id, "psps/stripe/live/acct_884_test/secret_key", "sk_1"); err != nil {
		t.Fatal(err)
	}
	backend.puts = 0 // reset; we only count cache-driven backend hits below

	cache, advance := withClock(t, backend, 45*time.Second)

	// First Get is a miss -> 1 backend read; second within TTL is served cached.
	for i := 0; i < 3; i++ {
		got, err := cache.Get(ctx, id, "psps/stripe/live/acct_884_test/secret_key")
		if err != nil || got.Value != "sk_1" {
			t.Fatalf("get %d: %v / %q", i, err, got.Value)
		}
	}
	if backend.gets != 1 {
		t.Fatalf("within TTL: backend gets = %d, want 1", backend.gets)
	}

	// Advance past TTL -> next Get re-reads the backend.
	advance(46 * time.Second)
	if _, err := cache.Get(ctx, id, "psps/stripe/live/acct_884_test/secret_key"); err != nil {
		t.Fatal(err)
	}
	if backend.gets != 2 {
		t.Fatalf("after expiry: backend gets = %d, want 2", backend.gets)
	}
}

func TestCachedSecretStore_PutRefreshesImmediately(t *testing.T) {
	ctx := context.Background()
	backend := newCountingStore()
	id := dbtest.TestMerchantID
	cache, _ := withClock(t, backend, 45*time.Second)

	if _, err := cache.Put(ctx, id, "psps/stripe/live/acct_884_test/secret_key", "sk_old"); err != nil {
		t.Fatal(err)
	}
	// Rotate through the cache; the new value must be visible with NO backend Get.
	if _, err := cache.Put(ctx, id, "psps/stripe/live/acct_884_test/secret_key", "sk_new"); err != nil {
		t.Fatal(err)
	}
	got, err := cache.Get(ctx, id, "psps/stripe/live/acct_884_test/secret_key")
	if err != nil {
		t.Fatal(err)
	}
	if got.Value != "sk_new" {
		t.Fatalf("value = %q, want sk_new (write-through refresh)", got.Value)
	}
	if backend.gets != 0 {
		t.Fatalf("Put should refresh cache without a backend Get; gets = %d", backend.gets)
	}
}

func TestCachedSecretStore_DeleteInvalidates(t *testing.T) {
	ctx := context.Background()
	backend := newCountingStore()
	id := dbtest.TestMerchantID
	cache, _ := withClock(t, backend, 45*time.Second)

	if _, err := cache.Put(ctx, id, "psps/stripe/live/acct_884_test/secret_key", "sk_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Get(ctx, id, "psps/stripe/live/acct_884_test/secret_key"); err != nil { // populate cache
		t.Fatal(err)
	}
	if err := cache.Delete(ctx, id, "psps/stripe/live/acct_884_test/secret_key"); err != nil {
		t.Fatal(err)
	}
	// After delete the cached copy must be gone -> a fresh backend read that 404s.
	if _, err := cache.Get(ctx, id, "psps/stripe/live/acct_884_test/secret_key"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("post-delete Get = %v, want ErrSecretNotFound", err)
	}
}

func TestCachedSecretStore_ErrorsNotCached(t *testing.T) {
	ctx := context.Background()
	backend := newCountingStore()
	id := dbtest.TestMerchantID
	cache, _ := withClock(t, backend, 45*time.Second)

	// Backend transiently unavailable: the error must propagate and NOT be cached.
	backend.getErr = ErrSecretBackendUnavailable
	if _, err := cache.Get(ctx, id, "psps/stripe/live/acct_884_test/secret_key"); !errors.Is(err, ErrSecretBackendUnavailable) {
		t.Fatalf("get = %v, want ErrSecretBackendUnavailable", err)
	}
	// Recovery: backend now returns a value; the cache must not be pinned on the error.
	backend.getErr = nil
	if _, err := backend.Put(ctx, id, "psps/stripe/live/acct_884_test/secret_key", "sk_ok"); err != nil {
		t.Fatal(err)
	}
	got, err := cache.Get(ctx, id, "psps/stripe/live/acct_884_test/secret_key")
	if err != nil || got.Value != "sk_ok" {
		t.Fatalf("after recovery: %v / %q", err, got.Value)
	}
}

func TestCachedSecretStore_DisabledPassesThrough(t *testing.T) {
	backend := newCountingStore()
	if got := NewCachedSecretStore(backend, 0); got != MerchantSecretStore(backend) {
		t.Fatalf("ttl<=0 should return inner unchanged, got %T", got)
	}
	if got := NewCachedSecretStore(backend, -time.Second); got != MerchantSecretStore(backend) {
		t.Fatalf("negative ttl should return inner unchanged, got %T", got)
	}
}

// --- error taxonomy: unreachable (retry) vs absent (terminal) -----------------

// errVaultKV is a VaultKV whose reads fail, simulating an unreachable/sealed Vault.
type errVaultKV struct{ err error }

func (e errVaultKV) ReadSecret(context.Context, string) (map[string]string, int, error) {
	return nil, 0, e.err
}
func (e errVaultKV) WriteSecret(context.Context, string, map[string]string) (int, error) {
	return 0, e.err
}
func (e errVaultKV) DeleteSecret(context.Context, string) error            { return e.err }
func (e errVaultKV) ListSecrets(context.Context, string) ([]string, error) { return nil, e.err }

func TestVaultSecretStore_UnreachableIsBackendUnavailableNotAbsent(t *testing.T) {
	ctx := context.Background()
	store := NewVaultSecretStore("secret", errVaultKV{err: errors.New("dial tcp: connection refused")})
	_, err := store.Get(ctx, dbtest.TestMerchantID, "psps/stripe/live/acct_884_test/secret_key")
	if !errors.Is(err, ErrSecretBackendUnavailable) {
		t.Fatalf("unreachable Get = %v, want wraps ErrSecretBackendUnavailable", err)
	}
	if errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("unreachable Get must NOT look like absence: %v", err)
	}
}

func TestVaultSecretStore_AbsentIsNotFound(t *testing.T) {
	ctx := context.Background()
	store := NewVaultSecretStore("secret", newFakeVaultKV()) // empty store -> no value
	_, err := store.Get(ctx, dbtest.TestMerchantID, "psps/stripe/live/acct_884_test/secret_key")
	if !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("absent Get = %v, want ErrSecretNotFound", err)
	}
	if errors.Is(err, ErrSecretBackendUnavailable) {
		t.Fatalf("absence must NOT look like a backend outage: %v", err)
	}
}

func TestVaultSecretStore_StubWrapsBackendUnavailable(t *testing.T) {
	// The not-yet-wired stub must fail closed AND be classified retryable, so a
	// managed deployment that forgot to wire Vault never reads "secret absent".
	if !errors.Is(ErrVaultNotConfigured, ErrSecretBackendUnavailable) {
		t.Fatal("ErrVaultNotConfigured must wrap ErrSecretBackendUnavailable")
	}
	_, err := NewVaultSecretStore("secret", nil).Get(context.Background(), dbtest.TestMerchantID, "psps/stripe/live/acct_884_test/secret_key")
	if !errors.Is(err, ErrSecretBackendUnavailable) {
		t.Fatalf("stub Get = %v, want wraps ErrSecretBackendUnavailable", err)
	}
}
