package ccbill

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// DataLinkInterval is how often DataLink answers one live account: CCBill
// allows a request once every 60 minutes; test mode is exempt.
const DataLinkInterval = time.Hour

// Slots keeps each CCBill account's next DataLink request slot, shared by
// every process that reads the account (abusestate.Store).
type Slots interface {
	// Claim takes key's slot when it is open at now and keeps it until
	// next; otherwise it answers when the slot opens.
	Claim(ctx context.Context, key string, now, next time.Time) (time.Time, bool)
	// Reopen opens key's slot at once.
	Reopen(ctx context.Context, key string)
}

// Pacing is where a live DataLink client takes its slots and its clock. A
// zero Pacing keeps the slots in this process and reads the wall clock.
type Pacing struct {
	Slots Slots
	Now   func() time.Time
}

// ErrDataLinkPaced is a live account's DataLink request that waits for the
// account's next slot: not a failure, and nothing was read.
var ErrDataLinkPaced = errors.New("ccbill datalink answers a live account once an hour")

// PacedError is a DataLink request not made yet, or refused by CCBill for
// coming within the hour: Until is when the account's next slot opens, Wait
// how long that was from the refusal.
type PacedError struct {
	Until time.Time
	Wait  time.Duration
}

func (e *PacedError) Error() string {
	return fmt.Sprintf("%v: next request at %s", ErrDataLinkPaced, e.Until.UTC().Format(time.RFC3339))
}

func (e *PacedError) Unwrap() error { return ErrDataLinkPaced }

// PacedFor reports how long err waits for a DataLink slot.
func PacedFor(err error) (time.Duration, bool) {
	var paced *PacedError
	if !errors.As(err, &paced) {
		return 0, false
	}
	return max(paced.Wait, time.Second), true
}

// Paced reports whether the client's requests take the account's hourly
// slot: a live account's. Test mode is not paced.
func (c *DataLinkClient) Paced() bool { return !c.DevMode }

// claimSlot takes the account's slot for the request about to be sent, or
// answers when it opens.
func (c *DataLinkClient) claimSlot(ctx context.Context) (time.Time, error) {
	now := c.now()
	next, ok := c.slots().Claim(ctx, c.slotKey(), now, now.Add(DataLinkInterval))
	if !ok {
		return next, &PacedError{Until: next, Wait: next.Sub(now)}
	}
	return next, nil
}

func (c *DataLinkClient) slotKey() string { return "ccbill:datalink:" + c.ClientAccNum }

func (c *DataLinkClient) now() time.Time {
	if c.Pacing.Now != nil {
		return c.Pacing.Now().UTC()
	}
	return time.Now().UTC()
}

func (c *DataLinkClient) slots() Slots {
	if c.Pacing.Slots != nil {
		return c.Pacing.Slots
	}
	return processSlots
}

// processSlots keeps the slots of clients given none: one process's
// requests stay an hour apart; other processes share only through Slots.
var processSlots Slots = &memorySlots{next: map[string]time.Time{}}

type memorySlots struct {
	mu   sync.Mutex
	next map[string]time.Time
}

func (m *memorySlots) Claim(_ context.Context, key string, now, next time.Time) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if held := m.next[key]; held.After(now) {
		return held, false
	}
	m.next[key] = next
	return next, true
}

func (m *memorySlots) Reopen(_ context.Context, key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.next, key)
}
