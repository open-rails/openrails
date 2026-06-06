package recurring

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
)

// fakePrepareRPC is a minimal prepareRPC for exercising the read-after-write
// retry in readAuthorityInitID (#274). GetAccountData returns `empties` empty
// reads, then `data` on every subsequent call; getErr (if set) short-circuits.
type fakePrepareRPC struct {
	empties int    // number of leading empty (not-yet-visible) reads
	data    []byte // authority bytes served once the empties are exhausted
	getErr  error  // hard RPC error to return on the first call

	calls int
}

func (f *fakePrepareRPC) GetAccountData(_ context.Context, _ solanago.PublicKey) ([]byte, error) {
	f.calls++
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.calls <= f.empties {
		return nil, nil
	}
	return f.data, nil
}

func (f *fakePrepareRPC) GetLatestBlockhash(context.Context) (solanago.Hash, error) {
	return solanago.Hash{}, nil
}

// authorityBytes builds a SubscriptionAuthority buffer whose i64 LE initId at the
// proven offset is `initID`.
func authorityBytes(initID int64) []byte {
	buf := make([]byte, subscriptionAuthorityInitIDOffset+8)
	binary.LittleEndian.PutUint64(buf[subscriptionAuthorityInitIDOffset:], uint64(initID))
	return buf
}

func withFastBackoff(t *testing.T) {
	t.Helper()
	prev := authorityReadBackoff
	authorityReadBackoff = time.Millisecond
	t.Cleanup(func() { authorityReadBackoff = prev })
}

// Settles after a few empty (read-after-write-lagged) reads and returns the right initId.
func TestReadAuthorityInitID_SettlesAfterEmptyReads(t *testing.T) {
	withFastBackoff(t)
	const want int64 = 424242
	rpc := &fakePrepareRPC{empties: 3, data: authorityBytes(want)}

	got, exists, err := readAuthorityInitID(context.Background(), rpc, solanago.PublicKey{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exists {
		t.Fatalf("expected authority to exist after retries")
	}
	if got != want {
		t.Fatalf("initId = %d, want %d", got, want)
	}
	if rpc.calls != 4 { // 3 empties + 1 settled read
		t.Fatalf("expected 4 reads, got %d", rpc.calls)
	}
}

// First read already has the account: no retry needed.
func TestReadAuthorityInitID_PresentImmediately(t *testing.T) {
	withFastBackoff(t)
	const want int64 = 7
	rpc := &fakePrepareRPC{empties: 0, data: authorityBytes(want)}

	got, exists, err := readAuthorityInitID(context.Background(), rpc, solanago.PublicKey{})
	if err != nil || !exists || got != want {
		t.Fatalf("got (%d,%v,%v), want (%d,true,nil)", got, exists, err, want)
	}
	if rpc.calls != 1 {
		t.Fatalf("expected a single read, got %d", rpc.calls)
	}
}

// Never present: stays empty across every attempt → treated as a genuinely absent
// authority (first-time subscriber), exists=false, no error.
func TestReadAuthorityInitID_NeverPresentIsFirstTime(t *testing.T) {
	withFastBackoff(t)
	rpc := &fakePrepareRPC{empties: 1000} // always empty

	got, exists, err := readAuthorityInitID(context.Background(), rpc, solanago.PublicKey{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exists {
		t.Fatalf("expected exists=false for an absent authority")
	}
	if got != 0 {
		t.Fatalf("expected initId 0, got %d", got)
	}
	if rpc.calls != authorityReadMaxAttempts {
		t.Fatalf("expected %d attempts, got %d", authorityReadMaxAttempts, rpc.calls)
	}
}

// Account appears but stays too short to read initId → never-settled error.
func TestReadAuthorityInitID_ShortAccountErrors(t *testing.T) {
	withFastBackoff(t)
	rpc := &fakePrepareRPC{empties: 0, data: make([]byte, subscriptionAuthorityInitIDOffset)} // 1 byte short

	_, exists, err := readAuthorityInitID(context.Background(), rpc, solanago.PublicKey{})
	if err == nil {
		t.Fatalf("expected an error when the account never reaches initId length")
	}
	if exists {
		t.Fatalf("expected exists=false on a never-settled short account")
	}
}

// A hard RPC error surfaces immediately.
func TestReadAuthorityInitID_RPCErrorSurfaces(t *testing.T) {
	withFastBackoff(t)
	sentinel := errors.New("boom")
	rpc := &fakePrepareRPC{getErr: sentinel}

	_, _, err := readAuthorityInitID(context.Background(), rpc, solanago.PublicKey{})
	if err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped RPC error, got %v", err)
	}
}

// A canceled context aborts the retry loop with the context error.
func TestReadAuthorityInitID_ContextCanceled(t *testing.T) {
	authorityReadBackoff = 50 * time.Millisecond
	t.Cleanup(func() { authorityReadBackoff = time.Second })

	ctx, cancel := context.WithCancel(context.Background())
	rpc := &fakePrepareRPC{empties: 1000} // forces a backoff wait on attempt 2
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	_, _, err := readAuthorityInitID(ctx, rpc, solanago.PublicKey{})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
