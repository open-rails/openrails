package solana

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Solana read-after-confirm staleness compensation. On a load-balanced RPC
// (Helius/Triton) or devnet, a read right after a confirm can hit a node that
// lags the write and return stale data (an old balance, a "missing" account):
//
//  1. minContextSlot: given the slot our write confirmed at
//     (TransactionOutcome.Slot), the *AtSlot reads ask the node to evaluate at
//     >= that slot; a lagging node errors ("Minimum context slot has not been
//     reached") instead of answering stale, and the read retries.
//  2. ReadUntilConsistent: with no confirmed slot (e.g. confirming a
//     wallet-signed subscribe we never submitted), poll until a predicate
//     holds or the bound is reached.

// defaultReadUntilConsistentAttempts / Backoff bound ReadUntilConsistent when
// the caller does not override them.
const (
	defaultReadUntilConsistentAttempts = 10
	defaultReadUntilConsistentBackoff  = time.Second
)

// ReadUntilConsistentOpts tunes the bounded read-until-consistent retry. Zero
// values fall back to the package defaults.
type ReadUntilConsistentOpts struct {
	// Attempts is the maximum number of reads (>= 1). Zero -> default.
	Attempts int
	// Backoff is the delay between reads. Zero -> default.
	Backoff time.Duration
}

func (o ReadUntilConsistentOpts) attempts() int {
	if o.Attempts > 0 {
		return o.Attempts
	}
	return defaultReadUntilConsistentAttempts
}

func (o ReadUntilConsistentOpts) backoff() time.Duration {
	if o.Backoff > 0 {
		return o.Backoff
	}
	return defaultReadUntilConsistentBackoff
}

// ReadUntilConsistent runs read() until ok() accepts its value, ctx ends, or the
// attempt bound is reached: compensation for reads with no confirmed slot to
// gate on. A read error counts as transient (a not-yet-visible account often
// surfaces as one) and is returned only once attempts run out. Exhaustion after
// a successful read returns the last value with a "never satisfied" error, so
// the caller can judge it.
func ReadUntilConsistent[T any](
	ctx context.Context,
	opts ReadUntilConsistentOpts,
	read func(context.Context) (T, error),
	ok func(T) bool,
) (T, error) {
	var (
		last    T
		lastErr error
		sawRead bool
	)
	attempts := opts.attempts()
	backoff := opts.backoff()
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return last, fmt.Errorf("solana: read-until-consistent canceled: %w", ctx.Err())
			case <-time.After(backoff):
			}
		}
		val, err := read(ctx)
		if err != nil {
			lastErr = err
			continue // transient: a lagging/missing account often surfaces as an error
		}
		sawRead = true
		last = val
		if ok(val) {
			return val, nil
		}
	}
	if !sawRead && lastErr != nil {
		return last, fmt.Errorf("solana: read-until-consistent never succeeded after %d attempts: %w", attempts, lastErr)
	}
	return last, fmt.Errorf("solana: read-until-consistent predicate never satisfied after %d attempts", attempts)
}

// isMinContextSlotError reports whether an RPC error says the node has not yet
// reached the requested minContextSlot. Retryable: a caught-up node answers.
// The message varies by node, so the match is loose.
func isMinContextSlotError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "minimum context slot") ||
		strings.Contains(msg, "min context slot") ||
		strings.Contains(msg, "context slot has not been reached")
}
