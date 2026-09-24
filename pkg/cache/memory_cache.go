package cache

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

type memoryCacheItem struct {
	value      []byte
	expiration time.Time
}

type MemoryCache struct {
	mu        sync.RWMutex
	items     map[string]*memoryCacheItem
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

func NewMemoryCache() *MemoryCache {
	mc := &MemoryCache{
		items: make(map[string]*memoryCacheItem),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}

	// Start cleanup goroutine
	go mc.cleanupExpired()

	return mc
}

func (c *MemoryCache) Get(ctx context.Context, key string, dest any) error {
	c.mu.RLock()
	item, exists := c.items[key]
	c.mu.RUnlock()

	if !exists {
		return nil // Key not found, return nil like Redis
	}

	// Check if expired
	if !item.expiration.IsZero() && time.Now().After(item.expiration) {
		c.Delete(ctx, key)
		return nil
	}

	return json.Unmarshal(item.value, dest)
}

func (c *MemoryCache) Set(ctx context.Context, key string, value any, expiration time.Duration) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}

	var exp time.Time
	if expiration > 0 {
		exp = time.Now().Add(expiration)
	}

	c.mu.Lock()
	c.items[key] = &memoryCacheItem{
		value:      data,
		expiration: exp,
	}
	c.mu.Unlock()

	return nil
}

func (c *MemoryCache) Delete(ctx context.Context, key string) error {
	c.mu.Lock()
	delete(c.items, key)
	c.mu.Unlock()
	return nil
}

func (c *MemoryCache) Clear(ctx context.Context) error {
	c.mu.Lock()
	c.items = make(map[string]*memoryCacheItem)
	c.mu.Unlock()
	return nil
}

func (c *MemoryCache) Close() error {
	if c == nil || c.stop == nil {
		return nil
	}
	c.closeOnce.Do(func() { close(c.stop) })
	<-c.done
	return nil
}

// cleanupExpired runs periodically to remove expired items
func (c *MemoryCache) cleanupExpired() {
	ticker := time.NewTicker(1 * time.Minute)
	defer close(c.done)
	defer ticker.Stop()

	for {
		select {
		case <-c.stop:
			return
		case now := <-ticker.C:

			c.mu.Lock()
			for key, item := range c.items {
				if !item.expiration.IsZero() && now.After(item.expiration) {
					delete(c.items, key)
				}
			}
			c.mu.Unlock()
		}
	}
}
