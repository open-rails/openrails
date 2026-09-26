// Package retry paces background reconnects to optional providers: full-jitter
// capped exponential backoff, forever, the same schedule hosts use in
// github.com/open-rails/helpers/deps.
package retry

import (
	"context"
	"math/rand/v2"
	"time"
)

const (
	Base = 500 * time.Millisecond
	Max  = 30 * time.Second
)

// Backoff returns a delay uniform in [0, min(max, base·2^attempt)] for the
// zero-based attempt.
func Backoff(attempt int, base, max time.Duration) time.Duration {
	ceil := max
	if attempt < 30 {
		if d := base << attempt; d > 0 && d < max {
			ceil = d
		}
	}
	return rand.N(ceil + 1) // #nosec G404 -- retry jitter, not security-sensitive
}

// Sleep waits d or until ctx ends; it reports whether ctx is still live.
func Sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Forever calls fn until it succeeds or ctx ends, sleeping Backoff(attempt,
// Base, Max) between failures. onErr, when set, sees every failure.
func Forever(ctx context.Context, fn func(context.Context) error, onErr func(attempt int, err error)) error {
	for attempt := 0; ; attempt++ {
		err := fn(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if onErr != nil {
			onErr(attempt, err)
		}
		if !Sleep(ctx, Backoff(attempt, Base, Max)) {
			return ctx.Err()
		}
	}
}
