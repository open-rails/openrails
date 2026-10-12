package app

import (
	"sync"

	"github.com/open-rails/openrails/billing"
)

// merchantChanges fans a merchant's in-process change out to its watchers.
type merchantChanges struct {
	mu       sync.RWMutex
	watchers []func(billing.MerchantID)
}

// OnMerchantChange calls watch after each change this process commits to a
// merchant's catalog, configuration or directory entry, so in-process read
// caches drop what they hold for it.
func (r *Runtime) OnMerchantChange(watch func(billing.MerchantID)) {
	r.changes.mu.Lock()
	defer r.changes.mu.Unlock()
	r.changes.watchers = append(r.changes.watchers, watch)
}

// MerchantChanged announces a committed change to id.
func (r *Runtime) MerchantChanged(id billing.MerchantID) {
	if r == nil {
		return
	}
	r.changes.mu.RLock()
	defer r.changes.mu.RUnlock()
	for _, watch := range r.changes.watchers {
		watch(id)
	}
}
