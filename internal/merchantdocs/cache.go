package merchantdocs

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
)

// Recheck is how often an access reloads a Vault-held merchant: the bound on
// how late an edit made outside OpenRails is seen. Edits through OpenRails
// reach every replica at once.
const Recheck = 30 * time.Second

// loadTimeout bounds a background reload.
const loadTimeout = 30 * time.Second

// SyncFunc reconciles a freshly loaded set with the identities Postgres
// records and returns the set to serve; it may move documents into
// Set.Rejected. previous is the set served before, zero on a first load.
type SyncFunc func(ctx context.Context, id billing.MerchantID, previous, loaded Set) (Set, error)

// Cache serves each merchant's configuration from memory, loaded on first
// use. Nothing sweeps every merchant: a Vault-held set reloads in the
// background when an access finds it older than Recheck, or when another
// replica announces an edit. A failed reload keeps serving the cached set, so
// payments ride out a brief Vault outage.
type Cache struct {
	source  Source
	recheck time.Duration
	now     func() time.Time
	base    context.Context
	stop    context.CancelFunc

	mu        sync.Mutex
	reconcile SyncFunc
	announce  func(context.Context, billing.MerchantID) error
	entries   map[billing.MerchantID]*entry
}

type entry struct {
	load       sync.Mutex
	mu         sync.RWMutex
	set        Set
	loaded     bool
	checked    time.Time
	refreshing atomic.Bool
	again      atomic.Bool
}

// NewCache serves source. A file never changes underneath, so only a
// writable source is rechecked.
func NewCache(source Source) *Cache {
	base, stop := context.WithCancel(context.Background())
	c := &Cache{source: source, now: time.Now, base: base, stop: stop, entries: map[billing.MerchantID]*entry{}}
	if source.Writable() {
		c.recheck = Recheck
	}
	return c
}

// Source is where the configuration lives.
func (c *Cache) Source() Source { return c.source }

// Writable reports whether the configuration can change at runtime.
func (c *Cache) Writable() bool { return c != nil && c.source.Writable() }

// SetSync installs the reconciliation run on every load.
func (c *Cache) SetSync(f SyncFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reconcile = f
}

// SetAnnounce installs how an edit is announced to the other replicas.
func (c *Cache) SetAnnounce(f func(context.Context, billing.MerchantID) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.announce = f
}

// Close stops background reloads.
func (c *Cache) Close() { c.stop() }

func (c *Cache) entry(id billing.MerchantID) *entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[id]
	if !ok {
		e = &entry{}
		c.entries[id] = e
	}
	return e
}

// Get is the merchant's configuration.
func (c *Cache) Get(ctx context.Context, id billing.MerchantID) (Set, error) {
	e := c.entry(id)
	e.mu.RLock()
	set, loaded, checked := e.set, e.loaded, e.checked
	e.mu.RUnlock()
	if loaded {
		if c.recheck > 0 && c.now().Sub(checked) >= c.recheck {
			c.refresh(id, e)
		}
		return set, nil
	}
	return c.load(ctx, id, e, false)
}

// Reload reads the merchant's configuration from its source now.
func (c *Cache) Reload(ctx context.Context, id billing.MerchantID) (Set, error) {
	return c.load(ctx, id, c.entry(id), true)
}

// Invalidate reloads the merchant in the background: another replica
// announced an edit.
func (c *Cache) Invalidate(id billing.MerchantID) {
	c.mu.Lock()
	e, ok := c.entries[id]
	c.mu.Unlock()
	if ok {
		c.refresh(id, e)
	}
}

// Forget drops the merchant: it was deleted.
func (c *Cache) Forget(id billing.MerchantID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, id)
}

func (c *Cache) load(ctx context.Context, id billing.MerchantID, e *entry, force bool) (Set, error) {
	e.load.Lock()
	defer e.load.Unlock()
	e.mu.RLock()
	previous, loaded := e.set, e.loaded
	e.mu.RUnlock()
	if loaded && !force {
		return previous, nil
	}
	set, err := c.source.Load(ctx, id)
	c.mu.Lock()
	reconcile := c.reconcile
	c.mu.Unlock()
	if err == nil && reconcile != nil {
		set, err = reconcile(ctx, id, previous, set)
	}
	now := c.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		if e.loaded {
			e.checked = now
		}
		return e.set, err
	}
	for path, why := range set.Rejected {
		if previous.Rejected[path] != why {
			log.WithFields(log.Fields{"merchant_id": id.String(), "document": path, "reason": why}).Error("merchant config: a document is not served")
		}
	}
	e.set, e.loaded, e.checked = set, true, now
	return set, nil
}

// refresh reloads in the background, once at a time; a request arriving
// during one runs another after it.
func (c *Cache) refresh(id billing.MerchantID, e *entry) {
	e.again.Store(true)
	if !e.refreshing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer e.refreshing.Store(false)
		for e.again.Swap(false) {
			if c.base.Err() != nil {
				return
			}
			ctx, cancel := context.WithTimeout(c.base, loadTimeout)
			_, err := c.load(ctx, id, e, true)
			cancel()
			if err != nil {
				log.WithError(err).WithField("merchant_id", id.String()).Warn("merchant config: reload failed; serving the cached configuration")
			}
		}
	}()
}

// write runs one document write, then reloads the merchant and announces it.
func (c *Cache) write(ctx context.Context, id billing.MerchantID, put func() error) (Set, error) {
	if err := put(); err != nil {
		return Set{}, err
	}
	set, err := c.Reload(ctx, id)
	if err != nil {
		return set, err
	}
	c.mu.Lock()
	announce := c.announce
	c.mu.Unlock()
	if announce != nil {
		if err := announce(ctx, id); err != nil {
			log.WithError(err).WithField("merchant_id", id.String()).Warn("merchant config: announcing the edit failed; other replicas see it within the recheck")
		}
	}
	return set, nil
}

// PutMerchant writes the merchant document at revision cas.
func (c *Cache) PutMerchant(ctx context.Context, id billing.MerchantID, doc Merchant, cas int64) (Set, error) {
	return c.write(ctx, id, func() error { _, err := c.source.PutMerchant(ctx, id, doc, cas); return err })
}

// PutPSP writes one PSP document at revision cas.
func (c *Cache) PutPSP(ctx context.Context, id billing.MerchantID, key string, doc PSP, cas int64) (Set, error) {
	return c.write(ctx, id, func() error { _, err := c.source.PutPSP(ctx, id, key, doc, cas); return err })
}

// PutCustodian writes one custodian document at revision cas.
func (c *Cache) PutCustodian(ctx context.Context, id billing.MerchantID, key string, doc Custodian, cas int64) (Set, error) {
	return c.write(ctx, id, func() error { _, err := c.source.PutCustodian(ctx, id, key, doc, cas); return err })
}

// Delete removes every document of the merchant.
func (c *Cache) Delete(ctx context.Context, id billing.MerchantID) error {
	if err := c.source.Delete(ctx, id); err != nil {
		return err
	}
	c.Forget(id)
	return nil
}

// MerchantSettings is the merchant document's display name and settings.
func (c *Cache) MerchantSettings(ctx context.Context, id billing.MerchantID) (string, billing.MerchantSettings, error) {
	set, err := c.Get(ctx, id)
	if err != nil {
		return "", billing.MerchantSettings{}, err
	}
	return set.Merchant.Value.DisplayName, set.Merchant.Value.Settings, nil
}
