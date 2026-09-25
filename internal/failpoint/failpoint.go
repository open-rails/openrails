// Package failpoint names the points of a charge where tests interleave
// replicas deterministically. With no hook set, Hit is one atomic load.
package failpoint

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
)

type Point string

const (
	AfterFence     Point = "after-fence"
	BeforeProvider Point = "before-provider"
	AfterProvider  Point = "after-provider"
	BeforeComplete Point = "before-complete"
	// ClaimLost fires when a lease heartbeat finds its claim gone.
	ClaimLost Point = "claim-lost"
)

// Site is one hit: the point, the operation and its subscription.
type Site struct {
	Point        Point
	Kind         string
	Operation    uuid.UUID
	Subscription uuid.UUID
}

// Hook runs at every hit. An error aborts the caller as a crash at that point
// would: work done so far stays, nothing after it happens.
type Hook func(context.Context, Site) error

var (
	armed atomic.Int64
	mu    sync.RWMutex
	next  int
	hooks = map[int]Hook{}
)

// Set installs a hook until the returned remove is called.
func Set(h Hook) (remove func()) {
	mu.Lock()
	next++
	id := next
	hooks[id] = h
	mu.Unlock()
	armed.Add(1)
	var once sync.Once
	return func() {
		once.Do(func() {
			mu.Lock()
			delete(hooks, id)
			mu.Unlock()
			armed.Add(-1)
		})
	}
}

// Hit runs every installed hook for site.
func Hit(ctx context.Context, site Site) error {
	if armed.Load() == 0 {
		return nil
	}
	mu.RLock()
	list := make([]Hook, 0, len(hooks))
	for _, h := range hooks {
		list = append(list, h)
	}
	mu.RUnlock()
	for _, h := range list {
		if err := h(ctx, site); err != nil {
			return err
		}
	}
	return nil
}
