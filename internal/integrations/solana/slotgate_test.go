package solana

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestReadUntilConsistent_ReturnsWhenPredicateHolds(t *testing.T) {
	t.Parallel()
	calls := 0
	// The "balance" is 0 for the first two reads (lagging node), then 5.
	read := func(ctx context.Context) (uint64, error) {
		calls++
		if calls < 3 {
			return 0, nil
		}
		return 5, nil
	}
	got, err := ReadUntilConsistent(context.Background(),
		ReadUntilConsistentOpts{Attempts: 5, Backoff: time.Millisecond},
		read, func(b uint64) bool { return b > 0 })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 5 {
		t.Fatalf("got %d, want 5", got)
	}
	if calls != 3 {
		t.Fatalf("expected 3 reads, got %d", calls)
	}
}

func TestReadUntilConsistent_FirstReadSucceedsNoBackoff(t *testing.T) {
	t.Parallel()
	calls := 0
	read := func(ctx context.Context) (uint64, error) { calls++; return 7, nil }
	start := time.Now()
	got, err := ReadUntilConsistent(context.Background(),
		// A large backoff that must NOT be incurred when the first read passes.
		ReadUntilConsistentOpts{Attempts: 5, Backoff: time.Hour},
		read, func(b uint64) bool { return b > 0 })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 7 || calls != 1 {
		t.Fatalf("got=%d calls=%d, want 7/1", got, calls)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("incurred backoff before the first read")
	}
}

func TestReadUntilConsistent_PredicateNeverSatisfied(t *testing.T) {
	t.Parallel()
	calls := 0
	read := func(ctx context.Context) (uint64, error) { calls++; return 0, nil }
	got, err := ReadUntilConsistent(context.Background(),
		ReadUntilConsistentOpts{Attempts: 3, Backoff: time.Millisecond},
		read, func(b uint64) bool { return b > 0 })
	if err == nil {
		t.Fatal("expected error when predicate never satisfied")
	}
	// Returns the LAST observed value so the caller can decide.
	if got != 0 {
		t.Fatalf("want last value 0, got %d", got)
	}
	if calls != 3 {
		t.Fatalf("expected 3 attempts, got %d", calls)
	}
}

func TestReadUntilConsistent_RetriesTransientErrors(t *testing.T) {
	t.Parallel()
	calls := 0
	read := func(ctx context.Context) (uint64, error) {
		calls++
		if calls < 2 {
			return 0, errors.New("could not find account")
		}
		return 9, nil
	}
	got, err := ReadUntilConsistent(context.Background(),
		ReadUntilConsistentOpts{Attempts: 4, Backoff: time.Millisecond},
		read, func(b uint64) bool { return b > 0 })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 9 {
		t.Fatalf("got %d, want 9", got)
	}
}

func TestReadUntilConsistent_AllErrorsSurfacesLastErr(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("rpc down")
	read := func(ctx context.Context) (uint64, error) { return 0, sentinel }
	_, err := ReadUntilConsistent(context.Background(),
		ReadUntilConsistentOpts{Attempts: 2, Backoff: time.Millisecond},
		read, func(b uint64) bool { return b > 0 })
	if err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel error, got %v", err)
	}
}

func TestReadUntilConsistent_RespectsContextCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	read := func(ctx context.Context) (uint64, error) {
		calls++
		cancel() // cancel after the first read so the next backoff aborts
		return 0, nil
	}
	_, err := ReadUntilConsistent(ctx,
		ReadUntilConsistentOpts{Attempts: 10, Backoff: 50 * time.Millisecond},
		read, func(b uint64) bool { return b > 0 })
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 read before cancel, got %d", calls)
	}
}

func TestReadUntilConsistent_DefaultsApplied(t *testing.T) {
	t.Parallel()
	if (ReadUntilConsistentOpts{}).attempts() != defaultReadUntilConsistentAttempts {
		t.Fatal("zero Attempts should fall back to the default")
	}
	if (ReadUntilConsistentOpts{}).backoff() != defaultReadUntilConsistentBackoff {
		t.Fatal("zero Backoff should fall back to the default")
	}
}

func TestIsMinContextSlotError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("Minimum context slot has not been reached"), true},
		{errors.New("minimum context slot has not been reached: served=10 want>=20"), true},
		{errors.New("Node is behind by 100 slots"), false},
		{errors.New("could not find account"), false},
	}
	for _, c := range cases {
		if got := isMinContextSlotError(c.err); got != c.want {
			t.Errorf("isMinContextSlotError(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestRetryMinContextSlot_RetriesLagThenSucceeds(t *testing.T) {
	orig := minContextSlotReadBackoff
	minContextSlotReadBackoff = time.Millisecond
	defer func() { minContextSlotReadBackoff = orig }()
	calls := 0
	read := func(ctx context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("minimum context slot has not been reached: served=1 want>=5")
		}
		return nil
	}
	// Shrink the package backoff isn't possible (const); rely on ctx not firing.
	if err := retryMinContextSlot(context.Background(), read); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 attempts, got %d", calls)
	}
}

func TestRetryMinContextSlot_NonLagErrorReturnsImmediately(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("invalid public key")
	calls := 0
	read := func(ctx context.Context) error { calls++; return sentinel }
	err := retryMinContextSlot(context.Background(), read)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("a non-lag error must not retry; got %d calls", calls)
	}
}
