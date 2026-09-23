package providerposture

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRegistryVerifiesOnceAndRetriesOnlyUnknownAfterBackoff(t *testing.T) {
	now := time.Unix(0, 0)
	r := NewRegistry(time.Minute)
	r.Now = func() time.Time { return now }
	var calls atomic.Int64
	verdict := Unknown
	check := func(context.Context) (Verdict, error) {
		calls.Add(1)
		if verdict == Unknown {
			return Unknown, errors.New("unavailable")
		}
		return verdict, nil
	}
	k := Key{Rail: "nmi", Credential: Fingerprint("a")}

	require.ErrorIs(t, r.Require(context.Background(), k, check), ErrDisarmed)
	require.ErrorIs(t, r.Require(context.Background(), k, check), ErrDisarmed)
	require.EqualValues(t, 1, calls.Load(), "unknown is not re-probed inside the backoff")

	verdict = Simulated
	now = now.Add(time.Minute)
	require.NoError(t, r.Require(context.Background(), k, check))
	now = now.Add(time.Hour)
	require.NoError(t, r.Require(context.Background(), k, check))
	require.EqualValues(t, 2, calls.Load(), "an armed verdict is never re-probed per mutation")

	verdict = Live
	require.True(t, r.Verify(context.Background(), k, check).Verdict == Live, "an explicit reload replaces the verdict")
	now = now.Add(time.Hour)
	require.ErrorIs(t, r.Require(context.Background(), k, check), ErrDisarmed)
	require.EqualValues(t, 3, calls.Load(), "live stays disarmed until the credential is reloaded")
}

func TestRegistrySingleflightsConcurrentFirstUse(t *testing.T) {
	r := NewRegistry(time.Minute)
	var calls atomic.Int64
	check := func(context.Context) (Verdict, error) {
		calls.Add(1)
		time.Sleep(10 * time.Millisecond)
		return Simulated, nil
	}
	k := Key{Rail: "stripe", Credential: Fingerprint("b")}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			require.NoError(t, r.Require(context.Background(), k, check))
		}()
	}
	wg.Wait()
	require.EqualValues(t, 1, calls.Load())
}

func TestKeyBindsEveryIdentityComponent(t *testing.T) {
	r := NewRegistry(time.Minute)
	base := Key{Rail: "nmi", AccountID: "1", Endpoint: "e", Credential: Fingerprint("k")}
	require.True(t, r.Verify(context.Background(), base, func(context.Context) (Verdict, error) { return Simulated, nil }).Armed())
	for _, other := range []Key{
		{Rail: "nmi", AccountID: "2", Endpoint: "e", Credential: Fingerprint("k")},
		{Rail: "nmi", AccountID: "1", Endpoint: "f", Credential: Fingerprint("k")},
		{Rail: "nmi", AccountID: "1", Endpoint: "e", Credential: Fingerprint("rotated")},
	} {
		_, seen := r.Lookup(other)
		require.False(t, seen)
	}
	require.NotEqual(t, Fingerprint("ab", "c"), Fingerprint("a", "bc"))
}

func TestTrackedReportsAndRecoversDisarmedKeys(t *testing.T) {
	now := time.Unix(0, 0)
	r := NewRegistry(time.Minute)
	r.Now = func() time.Time { return now }
	up := false
	check := func(context.Context) (Verdict, error) {
		if up {
			return Simulated, nil
		}
		return Unknown, errors.New("blip")
	}
	k := Key{Rail: "nmi", Credential: Fingerprint("c")}
	var tracked Tracked
	r.Verify(context.Background(), k, check)
	tracked.Add(k, check)
	require.Len(t, tracked.Disarmed(context.Background(), r), 1)
	up = true
	require.Len(t, tracked.Disarmed(context.Background(), r), 1, "still inside the retry backoff")
	now = now.Add(time.Minute)
	require.Empty(t, tracked.Disarmed(context.Background(), r), "a due retry re-arms without a restart")
}
