package intents

import "time"

// BackoffPolicy is a capped exponential backoff: Base, 2*Base, 4*Base, ...
// capped at Cap. attempts is the number of attempts already made (>= 1 when
// computing the delay after a failure).
type BackoffPolicy struct {
	Base time.Duration
	Cap  time.Duration
}

// DefaultBackoff suits provider mutations that should converge quickly but
// not hammer a struggling gateway: 2m, 4m, 8m, ... capped at 2h.
var DefaultBackoff = BackoffPolicy{Base: 2 * time.Minute, Cap: 2 * time.Hour}

// Delay returns the wait before the next attempt.
func (p BackoffPolicy) Delay(attempts int32) time.Duration {
	base := p.Base
	if base <= 0 {
		base = time.Minute
	}
	cap := p.Cap
	if cap <= 0 {
		cap = time.Hour
	}
	if attempts < 1 {
		attempts = 1
	}
	d := base
	for i := int32(1); i < attempts; i++ {
		d *= 2
		if d >= cap {
			return cap
		}
	}
	if d > cap {
		return cap
	}
	return d
}
