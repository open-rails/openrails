//go:build integration

package testkit

import (
	"context"
	"errors"
	"sync"
	"time"
)

// JournalEntry is the provider-neutral fact recorded by a test provider. It is
// intentionally a small observation contract: provider fakes can preserve
// idempotency, operation, and external-reference facts without making the
// focused suite depend on Stripe/NMI wire structs.
type JournalEntry struct {
	Provider       string
	Operation      string
	IdempotencyKey string
	ExternalID     string
	Outcome        string
	AmountMinor    int64
	Currency       string
	At             time.Time
	Metadata       map[string]string
}

// ProviderJournal records externally observable provider operations. A journal
// is an assertion seam, not a billing ledger: production code must never use it
// for authorization or payment state.
type ProviderJournal interface {
	Append(context.Context, JournalEntry) error
	Snapshot(context.Context) ([]JournalEntry, error)
}

// MemoryJournal is a race-safe in-memory ProviderJournal for focused tests and
// loopback provider fakes. Entries are copied on write and read, so a test
// cannot mutate an earlier observation through a retained map or slice.
type MemoryJournal struct {
	mu      sync.Mutex
	entries []JournalEntry
}

var errJournalClosed = errors.New("testkit provider journal is closed")

// Append records one provider operation in call order.
func (j *MemoryJournal) Append(ctx context.Context, entry JournalEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if j == nil {
		return errJournalClosed
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.entries = append(j.entries, cloneJournalEntry(entry))
	return nil
}

// Snapshot returns a stable copy of all observations in append order.
func (j *MemoryJournal) Snapshot(ctx context.Context) ([]JournalEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if j == nil {
		return nil, errJournalClosed
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]JournalEntry, len(j.entries))
	for i, entry := range j.entries {
		out[i] = cloneJournalEntry(entry)
	}
	return out, nil
}

// Reset clears all observations. It is useful when a focused test reuses one
// provider fixture for several subtests.
func (j *MemoryJournal) Reset() {
	if j == nil {
		return
	}
	j.mu.Lock()
	j.entries = nil
	j.mu.Unlock()
}

func cloneJournalEntry(entry JournalEntry) JournalEntry {
	if metadata := entry.Metadata; metadata != nil {
		entry.Metadata = make(map[string]string, len(metadata))
		for key, value := range metadata {
			entry.Metadata[key] = value
		}
	}
	return entry
}
