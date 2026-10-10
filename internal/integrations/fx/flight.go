package fx

import (
	"context"
	"sync"
	"time"
)

const (
	// maxRateAge rejects a rate published longer ago than this, checked when
	// fetched and again when served: a lagging CDN file is not a fresh rate.
	maxRateAge = 48 * time.Hour
	// inlineFetchTimeout bounds one request-path upstream read.
	inlineFetchTimeout = 10 * time.Second
	defaultNegativeTTL = 30 * time.Second
)

func staleRate(asOf, now time.Time) bool {
	return asOf.IsZero() || now.Sub(asOf) > maxRateAge
}

// flights makes request-path upstream reads single-flight per key, bounded,
// abandoned when the caller's ctx ends, and negatively cached for negativeTTL
// so a burst of quotes does not hammer a failing upstream.
type flights[T any] struct {
	mu          sync.Mutex
	calls       map[string]*flightCall[T]
	failed      map[string]failedFetch
	negativeTTL time.Duration
}

type flightCall[T any] struct {
	done   chan struct{}
	result T
	err    error
}

type failedFetch struct {
	until time.Time
	err   error
}

func newFlights[T any]() *flights[T] {
	return &flights[T]{calls: map[string]*flightCall[T]{}, failed: map[string]failedFetch{}, negativeTTL: defaultNegativeTTL}
}

func (f *flights[T]) do(ctx context.Context, key string, fetch func(context.Context) (T, error)) (T, error) {
	var zero T
	f.mu.Lock()
	if failed, ok := f.failed[key]; ok && time.Now().Before(failed.until) {
		f.mu.Unlock()
		return zero, failed.err
	}
	call := f.calls[key]
	if call == nil {
		call = &flightCall[T]{done: make(chan struct{})}
		f.calls[key] = call
		go func() {
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), inlineFetchTimeout)
			defer cancel()
			call.result, call.err = fetch(fctx)
			f.mu.Lock()
			delete(f.calls, key)
			if call.err != nil {
				f.failed[key] = failedFetch{until: time.Now().Add(f.negativeTTL), err: call.err}
			} else {
				delete(f.failed, key)
			}
			f.mu.Unlock()
			close(call.done)
		}()
	}
	f.mu.Unlock()
	select {
	case <-call.done:
		return call.result, call.err
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

// forget clears a remembered failure once a refresh succeeds.
func (f *flights[T]) forget(key string) {
	f.mu.Lock()
	delete(f.failed, key)
	f.mu.Unlock()
}
