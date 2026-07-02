//go:build integration

package integrationharness

import (
	"sync"
	"time"

	"github.com/jonboulle/clockwork"
)

// SettableClock is a clockwork.Clock whose inner clock can be swapped per test.
// Install ONE SettableClock at app construction (every service captures it) and
// swap the delegate to a FakeClock inside a test — no post-construction
// per-service clock fan-out needed, and one booted suite serves both real-time
// and fake-time tests. Swaps affect subsequent calls only: timers/tickers
// already created keep their original clock.
type SettableClock struct {
	mu    sync.RWMutex
	inner clockwork.Clock
}

// NewSettableClock returns a SettableClock delegating to inner (real clock when nil).
func NewSettableClock(inner clockwork.Clock) *SettableClock {
	if inner == nil {
		inner = clockwork.NewRealClock()
	}
	return &SettableClock{inner: inner}
}

// Set swaps the delegate clock (real clock when nil).
func (c *SettableClock) Set(inner clockwork.Clock) {
	if inner == nil {
		inner = clockwork.NewRealClock()
	}
	c.mu.Lock()
	c.inner = inner
	c.mu.Unlock()
}

// Get returns the current delegate.
func (c *SettableClock) Get() clockwork.Clock {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.inner
}

func (c *SettableClock) After(d time.Duration) <-chan time.Time { return c.Get().After(d) }
func (c *SettableClock) Sleep(d time.Duration)                  { c.Get().Sleep(d) }
func (c *SettableClock) Now() time.Time                         { return c.Get().Now() }
func (c *SettableClock) Since(t time.Time) time.Duration        { return c.Get().Since(t) }
func (c *SettableClock) Until(t time.Time) time.Duration        { return c.Get().Until(t) }
func (c *SettableClock) NewTicker(d time.Duration) clockwork.Ticker {
	return c.Get().NewTicker(d)
}
func (c *SettableClock) NewTimer(d time.Duration) clockwork.Timer { return c.Get().NewTimer(d) }
func (c *SettableClock) AfterFunc(d time.Duration, f func()) clockwork.Timer {
	return c.Get().AfterFunc(d, f)
}
