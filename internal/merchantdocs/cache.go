package merchantdocs

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/logx"
)

// Recheck is how often an access reloads a Vault-held merchant: the bound on
// how late an edit made outside OpenRails is seen. Edits through OpenRails
// reach every replica at once.
const Recheck = 30 * time.Second

// loadTimeout bounds a load.
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
	// reloads are the background reloads running; Close waits for them.
	reloads sync.WaitGroup

	mu             sync.Mutex
	reconcile      SyncFunc
	record         RecordFunc
	announce       func(context.Context, billing.MerchantID) error
	entries        map[billing.MerchantID]*entry
	admissions     *destructive.Admissions
	fingerprintKey []byte
	// hold refuses every load and write until Release.
	hold error
	Log  *slog.Logger
}

type entry struct {
	mu      sync.RWMutex
	set     Set
	loaded  bool
	checked time.Time
	// loading is the load running, begun the begun-th; failed is a first
	// load that failed, answered until loadRetry has passed.
	loading    *loading
	begun      uint64
	failed     error
	failedAt   time.Time
	refreshing atomic.Bool
	again      atomic.Bool
}

// loading is one load of a merchant's configuration, which its callers wait
// for.
type loading struct {
	seq  uint64
	done chan struct{}
	set  Set
	err  error
}

// loadRetry is how long a first load that failed answers its error before a
// caller starts another: a failure every caller would meet (a source or
// database out of reach) is not run again for each of them.
const loadRetry = 5 * time.Second

// NewCache serves source. A file never changes underneath, so only a
// writable source is rechecked.
func NewCache(logger *slog.Logger, source Source) *Cache {
	base, stop := context.WithCancel(context.Background())
	c := &Cache{source: source, now: time.Now, base: base, stop: stop, entries: map[billing.MerchantID]*entry{}, Log: logger}
	if source.Writable() {
		c.recheck = Recheck
	}
	return c
}

// Source is where the configuration lives.
func (c *Cache) Source() Source { return c.source }

// Hold refuses every load and write with why until Release: the source is
// not ready to serve (Vault has not answered, so neither the admission gate
// nor the fingerprint key is set).
func (c *Cache) Hold(why error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hold = why
}

// Release ends Hold.
func (c *Cache) Release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hold = nil
}

func (c *Cache) held() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hold == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, c.hold)
}

// Rejected names, for each loaded merchant, the documents it does not serve
// and why, and its undeclared accounts that still hold billing.
func (c *Cache) Rejected() map[billing.MerchantID]map[string]string {
	c.mu.Lock()
	entries := maps.Clone(c.entries)
	c.mu.Unlock()
	out := map[billing.MerchantID]map[string]string{}
	for id, e := range entries {
		e.mu.RLock()
		if len(e.set.Rejected)+len(e.set.Undeclared) > 0 {
			out[id] = maps.Clone(e.set.Rejected)
			if out[id] == nil {
				out[id] = map[string]string{}
			}
			maps.Copy(out[id], e.set.Undeclared)
		}
		e.mu.RUnlock()
	}
	return out
}

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

// Close stops background reloads and waits for any running.
func (c *Cache) Close() {
	c.mu.Lock()
	c.stop()
	c.mu.Unlock()
	c.reloads.Wait()
}

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

// Expire makes every cached merchant reload on its next use.
func (c *Cache) Expire() {
	c.mu.Lock()
	entries := make([]*entry, 0, len(c.entries))
	for _, e := range c.entries {
		entries = append(entries, e)
	}
	c.mu.Unlock()
	for _, e := range entries {
		e.mu.Lock()
		e.checked = time.Time{}
		e.mu.Unlock()
	}
}

// Forget drops the merchant: it was deleted.
func (c *Cache) Forget(id billing.MerchantID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, id)
}

// load reads the merchant's configuration from its source and reconciles it
// with PostgreSQL on the cache's own context, never the caller's: a load is
// the process's, so it neither joins the caller's transaction or connection
// nor ends when the caller gives up. Callers share the load running; a
// forced one waits for a load begun after it was asked for. Each caller waits
// within its own ctx.
func (c *Cache) load(ctx context.Context, id billing.MerchantID, e *entry, force bool) (Set, error) {
	e.mu.Lock()
	asked := e.begun
	for {
		if e.loaded && !force {
			set := e.set
			e.mu.Unlock()
			return set, nil
		}
		if !e.loaded && !force && e.failed != nil && c.now().Sub(e.failedAt) < loadRetry {
			set, err := e.set, e.failed
			e.mu.Unlock()
			return set, err
		}
		l := e.loading
		if l == nil {
			var err error
			if l, err = c.begin(id, e); err != nil {
				set := e.set
				e.mu.Unlock()
				return set, err
			}
		}
		e.mu.Unlock()
		select {
		case <-l.done:
		case <-ctx.Done():
			e.mu.RLock()
			set := e.set
			e.mu.RUnlock()
			return set, ctx.Err()
		}
		if !force || l.seq > asked {
			return l.set, l.err
		}
		// A forced load must read the source after it was asked for.
		e.mu.Lock()
	}
}

// begin starts a load of the merchant; e.mu is held.
func (c *Cache) begin(id billing.MerchantID, e *entry) (*loading, error) {
	c.mu.Lock()
	if c.base.Err() != nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: the merchant configuration cache is closed", ErrUnavailable)
	}
	c.reloads.Add(1)
	c.mu.Unlock()
	e.begun++
	l := &loading{seq: e.begun, done: make(chan struct{})}
	e.loading = l
	go func() {
		defer c.reloads.Done()
		ctx, cancel := context.WithTimeout(c.base, loadTimeout)
		defer cancel()
		l.set, l.err = c.loadNow(ctx, id, e)
		e.mu.Lock()
		e.loading = nil
		e.mu.Unlock()
		close(l.done)
	}()
	return l, nil
}

// loadNow is one load: the source read, then the reconciliation.
func (c *Cache) loadNow(ctx context.Context, id billing.MerchantID, e *entry) (Set, error) {
	e.mu.RLock()
	previous := e.set
	e.mu.RUnlock()
	if err := c.held(); err != nil {
		return previous, err
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
		} else {
			e.failed, e.failedAt = err, now
		}
		return e.set, err
	}
	for path, why := range set.Rejected {
		if previous.Rejected[path] != why {
			c.Log.ErrorContext(ctx, "merchant config: a document is not served", "merchant_id", id.String(), "document", path, "reason", why)
		}
	}
	for path, why := range set.Undeclared {
		if previous.Undeclared[path] != why {
			c.Log.WarnContext(ctx, "merchant config: an undeclared account still holds billing", "merchant_id", id.String(), "account", path, "reason", why)
		}
	}
	e.set, e.loaded, e.checked, e.failed = set, true, now, nil
	return set, nil
}

// refresh reloads in the background, once at a time; a request arriving
// during one runs another after it.
func (c *Cache) refresh(id billing.MerchantID, e *entry) {
	e.again.Store(true)
	if !e.refreshing.CompareAndSwap(false, true) {
		return
	}
	c.mu.Lock()
	if c.base.Err() != nil {
		c.mu.Unlock()
		e.refreshing.Store(false)
		return
	}
	c.reloads.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.reloads.Done()
		defer e.refreshing.Store(false)
		for e.again.Swap(false) {
			if c.base.Err() != nil {
				return
			}
			ctx, cancel := context.WithTimeout(c.base, loadTimeout)
			_, err := c.load(ctx, id, e, true)
			cancel()
			if err != nil {
				c.Log.WarnContext(ctx, "merchant config: reload failed; serving the cached configuration", logx.Err(err), "merchant_id", id.String())
			}
		}
	}()
}

// write runs one document write, then reloads the merchant and announces it.
func (c *Cache) write(ctx context.Context, id billing.MerchantID, put func() error) (Set, error) {
	if err := c.held(); err != nil {
		return Set{}, err
	}
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
			c.Log.WarnContext(ctx, "merchant config: announcing the edit failed; other replicas see it within the recheck", logx.Err(err), "merchant_id", id.String())
		}
	}
	return set, nil
}

// PutMerchant writes the merchant document at revision cas.
func (c *Cache) PutMerchant(ctx context.Context, id billing.MerchantID, doc Merchant, cas int64) (Set, error) {
	return c.admittedWrite(ctx, id, MerchantDoc, doc, cas, func() (int64, error) {
		written, err := c.source.PutMerchant(ctx, id, doc, cas)
		return written.Revision, err
	})
}

// PutPSP writes one PSP document at revision cas.
func (c *Cache) PutPSP(ctx context.Context, id billing.MerchantID, key string, doc PSP, cas int64) (Set, error) {
	return c.admittedWrite(ctx, id, PSPDoc(key), doc, cas, func() (int64, error) {
		written, err := c.source.PutPSP(ctx, id, key, doc, cas)
		return written.Revision, err
	})
}

// PutCustodian writes one custodian document at revision cas.
func (c *Cache) PutCustodian(ctx context.Context, id billing.MerchantID, key string, doc Custodian, cas int64) (Set, error) {
	return c.admittedWrite(ctx, id, CustodianDoc(key), doc, cas, func() (int64, error) {
		written, err := c.source.PutCustodian(ctx, id, key, doc, cas)
		return written.Revision, err
	})
}

// Delete removes every document of the merchant.
func (c *Cache) Delete(ctx context.Context, id billing.MerchantID) error {
	if err := c.held(); err != nil {
		return err
	}
	if err := c.source.Delete(ctx, id); err != nil {
		return err
	}
	c.Forget(id)
	return nil
}

// MerchantMessages is the messages document the merchant serves; none is
// the zero settings.
func (c *Cache) MerchantMessages(ctx context.Context, id billing.MerchantID) (billing.MessageSettings, error) {
	set, err := c.Get(ctx, id)
	if err != nil {
		return billing.MessageSettings{}, err
	}
	return set.Messages.Value, nil
}

// MerchantSettings is the merchant document's display name and settings.
// StoredCurrencies are the units the merchant's document declares in the
// source now, read past the cache (db.MerchantConfig).
func (c *Cache) StoredCurrencies(ctx context.Context, id billing.MerchantID) ([]billing.MerchantCurrency, error) {
	set, err := c.source.Load(ctx, id)
	if err != nil || !set.HasMerchant {
		return nil, err
	}
	return set.Merchant.Value.Currencies, nil
}

func (c *Cache) MerchantSettings(ctx context.Context, id billing.MerchantID) (string, billing.MerchantSettings, error) {
	set, err := c.Get(ctx, id)
	if err != nil {
		return "", billing.MerchantSettings{}, err
	}
	return set.Merchant.Value.DisplayName, set.Merchant.Value.MerchantSettings, nil
}
