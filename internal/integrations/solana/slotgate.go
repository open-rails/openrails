package solana

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Solana RPC read-after-write / read-after-confirm staleness compensation.
//
// On devnet and any load-balanced RPC (Helius/Triton), a read issued right after
// a transaction confirms can land on a DIFFERENT backend node that still LAGS the
// just-confirmed write, returning STALE data (an old balance, or an account that
// "doesn't exist yet"). Two mechanisms here guard against that:
//
//  1. minContextSlot — when we know the slot our write confirmed at (carried by
//     TransactionOutcome.Slot), we ask the node to evaluate the read at >= that
//     slot. A lagging node returns an RPC error ("Minimum context slot has not
//     been reached") INSTEAD of stale data, so we retry until a caught-up node
//     answers. The *AtSlot read variants below pass this through.
//
//  2. ReadUntilConsistent — for reads where we do NOT have a confirmed slot to
//     gate against (e.g. the server confirms a wallet-signed subscribe it never
//     submitted, so it never saw that tx's slot), we poll the read until a
//     caller-supplied predicate is satisfied or a deadline elapses. This is the
//     production analogue of the test-side awaitTokenCredit poller and of the
//     readAuthorityInitID bounded retry.

// defaultReadUntilConsistentAttempts / Backoff bound ReadUntilConsistent when the
// caller does not override them. ~10 attempts ~1s apart mirrors the
// readAuthorityInitID pacing and the test awaitTokenCredit poller.
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

// ReadUntilConsistent runs read() repeatedly until ok() accepts its value, ctx is
// cancelled, or the attempt bound is reached. It is the generic, ctx-aware
// read-after-write compensation for reads that have no confirmed slot to gate on.
//
// Semantics:
//   - returns (value, nil) the first time ok(value) is true;
//   - if read() errors, that attempt is retried (the error is treated as
//     transient — a not-yet-visible account commonly surfaces as an RPC error)
//     and the last error is returned only if every attempt is exhausted;
//   - on timeout/exhaustion with at least one successful (but not-yet-accepted)
//     read, returns the LAST observed value with a "never satisfied" error so the
//     caller can decide whether the last value is usable;
//   - respects ctx between attempts.
//
// T is the read's value type (e.g. uint64 for a balance, []byte for account data).
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
				return last, fmt.Errorf("solana: read-until-consistent cancelled: %w", ctx.Err())
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

// isMinContextSlotError reports whether an RPC error is a node telling us it has
// not yet reached the requested minContextSlot (i.e. it LAGS our confirmed write).
// Such errors are RETRYABLE: a caught-up node will answer. The exact message
// varies by node ("Minimum context slot has not been reached"), so we match
// loosely.
func isMinContextSlotError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "minimum context slot") ||
		strings.Contains(msg, "min context slot") ||
		strings.Contains(msg, "context slot has not been reached")
}
