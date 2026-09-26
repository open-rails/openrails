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

// flights makes request-path upstream reads single-flight per pair, bounded,
// abandoned when the caller's ctx ends, and negatively cached for negativeTTL
// so a burst of quotes does not hammer a failing upstream.
type flights struct {
	mu          sync.Mutex
	calls       map[string]*flightCall
	failed      map[string]failedFetch
	negativeTTL time.Duration
}

type flightCall struct {
	done  chan struct{}
	quote *Quote
	err   error
}

type failedFetch struct {
	until time.Time
	err   error
}

func newFlights() *flights {
	return &flights{calls: map[string]*flightCall{}, failed: map[string]failedFetch{}, negativeTTL: defaultNegativeTTL}
}

func (f *flights) do(ctx context.Context, key string, fetch func(context.Context) (*Quote, error)) (*Quote, error) {
	f.mu.Lock()
	if failed, ok := f.failed[key]; ok && time.Now().Before(failed.until) {
		f.mu.Unlock()
		return nil, failed.err
	}
	call := f.calls[key]
	if call == nil {
		call = &flightCall{done: make(chan struct{})}
		f.calls[key] = call
		go func() {
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), inlineFetchTimeout)
			defer cancel()
			call.quote, call.err = fetch(fctx)
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
		return call.quote, call.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// forget clears a remembered failure once a background refresh succeeds.
func (f *flights) forget(key string) {
	f.mu.Lock()
	delete(f.failed, key)
	f.mu.Unlock()
}
