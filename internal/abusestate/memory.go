package abusestate

import (
	"container/heap"
	"sync"
	"time"
)

// table is a bounded map of expiring entries. A heap orders them by expiry,
// so each call drops only the entries that have expired since the last one,
// and a full table drops the entry closest to expiry: never a pass over every
// key.
type table struct {
	mu      sync.Mutex
	limit   int
	entries map[string]*entry
	expiry  expiryHeap
}

type entry struct {
	key     string
	value   int64
	expires time.Time
	index   int
}

func newTable(limit int) *table {
	return &table{limit: limit, entries: map[string]*entry{}}
}

// add adds n to key's value, starting at n until expires when key is absent.
func (t *table) add(key string, n int64, expires, now time.Time) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweep(now)
	if e, ok := t.entries[key]; ok {
		e.value += n
		return e.value
	}
	t.insert(key, n, expires)
	return n
}

// set holds key until expires.
func (t *table) set(key string, expires, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweep(now)
	if e, ok := t.entries[key]; ok {
		e.expires = expires
		heap.Fix(&t.expiry, e.index)
		return
	}
	t.insert(key, 1, expires)
}

// left is how long key has before it expires; zero when absent.
func (t *table) left(key string, now time.Time) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweep(now)
	if e, ok := t.entries[key]; ok {
		return e.expires.Sub(now)
	}
	return 0
}

func (t *table) remove(keys []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, key := range keys {
		if e, ok := t.entries[key]; ok {
			heap.Remove(&t.expiry, e.index)
			delete(t.entries, key)
		}
	}
}

func (t *table) len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.entries)
}

func (t *table) insert(key string, value int64, expires time.Time) {
	for len(t.entries) >= t.limit {
		delete(t.entries, heap.Pop(&t.expiry).(*entry).key)
	}
	e := &entry{key: key, value: value, expires: expires}
	heap.Push(&t.expiry, e)
	t.entries[key] = e
}

// sweep drops the entries expired by now.
func (t *table) sweep(now time.Time) {
	for len(t.expiry) > 0 && !t.expiry[0].expires.After(now) {
		delete(t.entries, heap.Pop(&t.expiry).(*entry).key)
	}
}

type expiryHeap []*entry

func (h expiryHeap) Len() int           { return len(h) }
func (h expiryHeap) Less(i, j int) bool { return h[i].expires.Before(h[j].expires) }
func (h expiryHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index, h[j].index = i, j
}
func (h *expiryHeap) Push(x any) {
	e := x.(*entry)
	e.index = len(*h)
	*h = append(*h, e)
}
func (h *expiryHeap) Pop() any {
	old := *h
	e := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	return e
}
