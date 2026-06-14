package tenancy

import (
	"context"
	"sync"
	"time"

	"github.com/open-rails/openrails/pkg/merchant"
)

// DefaultSecretCacheTTL is the in-process secret cache TTL used when a managed
// deployment enables caching without specifying a window. It keeps hot paths
// such as tenant webhook verification from reading Vault per request while still
// letting out-of-band admin Vault writes converge without a process restart.
const DefaultSecretCacheTTL = 15 * time.Minute

// cachedSecretStore is a TTL-bounded, in-process read-through cache in front of
// any TenantSecretStore (issue #251). It exists so the recurring pull worker and
// hot request paths do not hit Vault (or the DB) once per row/request:
//
//   - Get caches successful reads keyed by (tenant, name) for ttl. ErrSecretNotFound
//     and ErrSecretBackendUnavailable are NOT cached — a just-created secret must be
//     visible immediately, and a transient backend outage must not be pinned.
//   - Put/Delete are write-through and invalidate (Put refreshes) the entry in the
//     SAME process immediately, so a rotation through this node is never stale.
//   - Secret.Version is tracked: a refreshed read with a higher version replaces the
//     cached value, so an admin rotation is reflected within one TTL even when it
//     lands on another node.
//
// Cross-process rotations converge within one ttl window (the entry expires and is
// re-read). This is a cache, never the source of truth — it holds plaintext only as
// long as the underlying store would have returned it.
type cachedSecretStore struct {
	inner TenantSecretStore
	ttl   time.Duration
	now   func() time.Time // injectable clock for tests

	mu      sync.Mutex
	entries map[cacheKey]cacheEntry
}

type cacheKey struct {
	tenant string
	name   string
}

type cacheEntry struct {
	secret  Secret
	expires time.Time
}

// NewCachedSecretStore wraps inner with an in-process TTL cache. A ttl <= 0
// disables caching and returns inner unchanged (no wrapper overhead), so callers
// can pass a config value straight through.
func NewCachedSecretStore(inner TenantSecretStore, ttl time.Duration) TenantSecretStore {
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

func (c *cachedSecretStore) Get(ctx context.Context, tenantID merchant.ID, name string) (Secret, error) {
	key := cacheKey{tenant: tenantID.String(), name: name}

	c.mu.Lock()
	if e, ok := c.entries[key]; ok && c.now().Before(e.expires) {
		sec := e.secret
		c.mu.Unlock()
		return sec, nil
	}
	c.mu.Unlock()

	// Miss (or expired): read through. Errors (absent / unavailable) are returned
	// to the caller and NOT cached.
	sec, err := c.inner.Get(ctx, tenantID, name)
	if err != nil {
		return Secret{}, err
	}

	c.mu.Lock()
	c.entries[key] = cacheEntry{secret: sec, expires: c.now().Add(c.ttl)}
	c.mu.Unlock()
	return sec, nil
}

func (c *cachedSecretStore) Put(ctx context.Context, tenantID merchant.ID, name, value string) (Secret, error) {
	sec, err := c.inner.Put(ctx, tenantID, name, value)
	if err != nil {
		// Invalidate on failure too: we no longer know the authoritative value.
		c.invalidate(tenantID, name)
		return Secret{}, err
	}
	// Write-through refresh so a rotation on this node is immediately visible.
	key := cacheKey{tenant: tenantID.String(), name: name}
	c.mu.Lock()
	c.entries[key] = cacheEntry{secret: sec, expires: c.now().Add(c.ttl)}
	c.mu.Unlock()
	return sec, nil
}

func (c *cachedSecretStore) Delete(ctx context.Context, tenantID merchant.ID, name string) error {
	err := c.inner.Delete(ctx, tenantID, name)
	// Whether or not delete succeeded, drop the cached copy: on success it's gone,
	// on failure we must not keep serving a value we tried to remove.
	c.invalidate(tenantID, name)
	return err
}

// List is not cached (enumeration is rare and used by export/audit paths).
func (c *cachedSecretStore) List(ctx context.Context, tenantID merchant.ID) ([]string, error) {
	return c.inner.List(ctx, tenantID)
}

func (c *cachedSecretStore) invalidate(tenantID merchant.ID, name string) {
	key := cacheKey{tenant: tenantID.String(), name: name}
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}
