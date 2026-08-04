package money

import (
	"errors"
	"fmt"
)

// The idempotency key on a money write is (Source, SourceID) within
// (merchant, payer, currency). Two rules hold everywhere in this package, with
// no exceptions and no "degrade quietly" path (or#891):
//
//  1. BOTH halves are REQUIRED. A money write without a key used to skip its
//     dedupe read and post unconditionally, so every retry double-charged. An
//     absent key is now a refusal, never a silent non-guarantee.
//  2. A key is a claim about the BODY, not just about the call. Replaying a key
//     with different charging terms is a caller bug, so it returns
//     ErrIdempotencyKeyReused instead of the first result. Answering a changed
//     retry with the original amount is how a corrected charge silently keeps
//     the wrong number.
//
// The key must be REPRODUCIBLE by the caller across retries of the same logical
// operation. A value minted per attempt (uuid.New() in a handler) satisfies
// every check here and guarantees nothing — it is a fresh operation each time.
var ErrIdempotencyKeyReused = errors.New("idempotency_key_reused")

// IdempotencyConflict reports a reused (Source, SourceID) whose retry carries
// different charging terms than the write that key already committed.
type IdempotencyConflict struct {
	Operation string // the money write that refused: "spend", "capture", "withdraw"
	Source    string
	SourceID  string
	Field     string // the charging term that differs
	Committed int64  // what the key already committed
	Retried   int64  // what this retry asked for
}

func (e *IdempotencyConflict) Error() string {
	return fmt.Sprintf(
		"%s: %s replayed idempotency key (%s, %s) with %s=%d, but that key already committed %s=%d",
		ErrIdempotencyKeyReused, e.Operation, e.Source, e.SourceID, e.Field, e.Retried, e.Field, e.Committed,
	)
}

func (e *IdempotencyConflict) Unwrap() error { return ErrIdempotencyKeyReused }

// requireIdempotencyKey rejects a blank key half. Callers trim first.
func requireIdempotencyKey(operation, source, sourceID string) error {
	if source == "" || sourceID == "" {
		return fmt.Errorf("%s: source and source_id required for idempotency", operation)
	}
	return nil
}
