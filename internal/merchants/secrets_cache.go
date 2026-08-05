package merchants

import (
	"context"
	"sync"
	"time"

	"github.com/open-rails/openrails/pkg/merchant"
)

// DefaultSecretCacheTTL is the in-process secret cache TTL used when a managed
// deployment enables caching without specifying a window. It keeps hot paths
// such as merchant webhook verification from reading Vault per request while still
// letting out-of-band admin Vault writes converge without a process restart.
const DefaultSecretCacheTTL = 15 * time.Minute

// cachedSecretStore is a TTL-bounded, in-process read-through cache in front of
// any MerchantSecretStore (issue #251). It exists so the recurring pull worker and
// hot request paths do not hit Vault (or the DB) once per row/request:
//
//   - Get caches successful reads keyed by (merchant, name) for ttl. ErrSecretNotFound
//     and ErrSecretBackendUnavailable are NOT cached — a just-created secret must be
//     visible immediately, and a transient backend outage must not be pinned.
//   - Put/Delete are write-through and invalidate (Put refreshes) the entry in the
//     SAME process immediately, so a rotation through this node is never stale.
//   - GetAtLeastVersion (or#812) is the cross-node cutover: a caller that knows the
//     credential has been rotated to version N — because the shared PSP row says so —
//     never gets a pre-N cache entry back. That makes a rotation on ANY node
//     effective on every other node at its next credential read, not one TTL later.
//
// TTL expiry remains the backstop for reads with no recorded version floor. This is
// a cache, never the source of truth — it holds plaintext only as long as the
// underlying store would have returned it.
type cachedSecretStore struct {
	inner MerchantSecretStore
	ttl   time.Duration
	now   func() time.Time // injectable clock for tests

	mu      sync.Mutex
	entries map[cacheKey]cacheEntry
}

type cacheKey struct {
	merchant string
	name     string
}

type cacheEntry struct {
	secret  Secret
	expires time.Time
}

// NewCachedSecretStore wraps inner with an in-process TTL cache. A ttl <= 0
// disables caching and returns inner unchanged (no wrapper overhead), so callers
// can pass a config value straight through.
func NewCachedSecretStore(inner MerchantSecretStore, ttl time.Duration) MerchantSecretStore {
	if ttl <= 0 {
		return inner
	}
	return &cachedSecretStore{
		inner:   inner,
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[cacheKey]cacheEntry),
	}
}

func (c *cachedSecretStore) Get(ctx context.Context, merchantID merchant.ID, name string) (Secret, error) {
	return c.GetAtLeastVersion(ctx, merchantID, name, 0)
}

// GetAtLeastVersion implements VersionedSecretReader: it is the cross-node
// rotation cutover (or#812). A cached entry is only usable when it is BOTH
// unexpired AND at least minVersion; a caller that learned from the shared PSP
// row that the credential has been rotated to version N is never answered from
// a pre-N cache entry, however much TTL is left. minVersion 0 is the
// unversioned path and behaves exactly as before.
func (c *cachedSecretStore) GetAtLeastVersion(ctx context.Context, merchantID merchant.ID, name string, minVersion int) (Secret, error) {
	key := cacheKey{merchant: merchantID.String(), name: name}

	c.mu.Lock()
	if e, ok := c.entries[key]; ok && c.now().Before(e.expires) && e.secret.Version >= minVersion {
		sec := e.secret
		c.mu.Unlock()
		return sec, nil
	}
	c.mu.Unlock()

	// Miss (or expired, or superseded): read through. Errors (absent /
	// unavailable) are returned to the caller and NOT cached.
	sec, err := c.inner.Get(ctx, merchantID, name)
	if err != nil {
		return Secret{}, err
	}

	if sec.Version < minVersion {
		// The backend has not caught up with a version the shared PSP row says
		// is committed. Serve what the backend authoritatively holds — refusing
		// would take a live merchant down over a replication lag — but do NOT
		// cache it, so the very next call retries instead of pinning the stale
		// value for a whole TTL.
		return sec, nil
	}

	c.mu.Lock()
	c.entries[key] = cacheEntry{secret: sec, expires: c.now().Add(c.ttl)}
	c.mu.Unlock()
	return sec, nil
}

func (c *cachedSecretStore) Put(ctx context.Context, merchantID merchant.ID, name, value string) (Secret, error) {
	sec, err := c.inner.Put(ctx, merchantID, name, value)
	if err != nil {
		// Invalidate on failure too: we no longer know the authoritative value.
		c.invalidate(merchantID, name)
		return Secret{}, err
	}
	// Write-through refresh so a rotation on this node is immediately visible.
	key := cacheKey{merchant: merchantID.String(), name: name}
	c.mu.Lock()
	c.entries[key] = cacheEntry{secret: sec, expires: c.now().Add(c.ttl)}
	c.mu.Unlock()
	return sec, nil
}

func (c *cachedSecretStore) Delete(ctx context.Context, merchantID merchant.ID, name string) error {
	err := c.inner.Delete(ctx, merchantID, name)
	// Whether or not delete succeeded, drop the cached copy: on success it's gone,
	// on failure we must not keep serving a value we tried to remove.
	c.invalidate(merchantID, name)
	return err
}

// List is not cached (enumeration is rare and used by export/audit paths).
func (c *cachedSecretStore) List(ctx context.Context, merchantID merchant.ID) ([]string, error) {
	return c.inner.List(ctx, merchantID)
}

func (c *cachedSecretStore) invalidate(merchantID merchant.ID, name string) {
	key := cacheKey{merchant: merchantID.String(), name: name}
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}
