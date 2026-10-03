package cache

import (
	"context"
	"sync/atomic"
	"time"
)

// holder wraps the Cache to keep a consistent concrete type in atomic.Value.
type holder struct{ c Cache }

// SwitchableCache proxies to an underlying Cache that can be swapped at runtime.
type SwitchableCache struct {
	current atomic.Value // stores *holder
}

func NewSwitchableCache(initial Cache) *SwitchableCache {
	sc := &SwitchableCache{}
	sc.current.Store(&holder{c: initial})
	return sc
}

func (s *SwitchableCache) SetBackend(c Cache) { s.current.Store(&holder{c: c}) }

func (s *SwitchableCache) backend() Cache {
	if v := s.current.Load(); v != nil {
		if h, ok := v.(*holder); ok && h.c != nil {
			return h.c
		}
	}
	return nil
}

func (s *SwitchableCache) Get(ctx context.Context, key string, dest any) error {
	return s.backend().Get(ctx, key, dest)
}

func (s *SwitchableCache) Set(ctx context.Context, key string, value any, expiration time.Duration) error {
	return s.backend().Set(ctx, key, value, expiration)
}

func (s *SwitchableCache) Delete(ctx context.Context, key string) error {
	return s.backend().Delete(ctx, key)
}
func (s *SwitchableCache) Clear(ctx context.Context) error { return s.backend().Clear(ctx) }
func (s *SwitchableCache) Close() error                    { return s.backend().Close() }
