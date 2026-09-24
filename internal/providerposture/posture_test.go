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

func fixed(v Verdict, err error) Check {
	return func(context.Context) (Verdict, error) { return v, err }
}

// Only the posture's expected verdict arms: sandbox needs Simulated, live (SEC-33)
// needs Live. A "matching" verdict that came with an error is not proof.
func TestOnlyExpectedVerdictArms(t *testing.T) {
	blip := errors.New("blip")
	for _, tc := range []struct {
		expect Verdict
		check  Check
		armed  bool
	}{
		{Unknown, fixed(Simulated, nil), true}, // zero Expect means sandbox
		{Simulated, fixed(Simulated, nil), true},
		{Simulated, fixed(Simulated, blip), false},
		{Simulated, fixed(Live, nil), false},
		{Simulated, fixed(Mismatched, nil), false},
		{Simulated, fixed(Unsupported, nil), false},
		{Simulated, fixed(Unknown, blip), false},
		{Simulated, nil, false},
		{Live, fixed(Live, nil), true},
		{Live, fixed(Simulated, nil), false},
		{Live, fixed(Live, blip), false},
	} {
		k := Key{Rail: "nmi", Credential: Fingerprint("k"), Expect: tc.expect}
		s := NewRegistry(time.Minute).Verify(context.Background(), k, tc.check)
		require.Equal(t, tc.armed, s.Armed(), "expect %s got %s", tc.expect, s.Verdict)
		if tc.armed {
			require.NoError(t, s.Error())
		} else {
			require.ErrorIs(t, s.Error(), ErrDisarmed)
		}
	}
	s := NewRegistry(time.Minute).Verify(context.Background(), Key{}, fixed(Unknown, blip))
	require.ErrorIs(t, s.Error(), blip, "the provider's reason is kept")
}

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
	ctx := context.Background()
	k := Key{Rail: "nmi", Credential: Fingerprint("a")}

	require.ErrorIs(t, r.Require(ctx, k, check), ErrDisarmed)
	require.ErrorIs(t, r.Require(ctx, k, check), ErrDisarmed)
	require.EqualValues(t, 1, calls.Load(), "unknown is not re-probed inside the backoff")

	verdict = Simulated
	now = now.Add(time.Minute)
	require.NoError(t, r.Require(ctx, k, check))
	now = now.Add(time.Hour)
	require.NoError(t, r.Require(ctx, k, check))
	require.EqualValues(t, 2, calls.Load(), "an armed verdict is never re-probed per mutation")

	verdict = Live
	require.Equal(t, Live, r.Verify(ctx, k, check).Verdict, "an explicit reload replaces the verdict")
	now = now.Add(time.Hour)
	require.ErrorIs(t, r.Require(ctx, k, check), ErrDisarmed)
	require.Equal(t, Live, r.Refresh(ctx, k, check).Verdict)
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
	errs := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- r.Require(context.Background(), k, check)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, calls.Load())
}

func TestKeyBindsEveryIdentityComponent(t *testing.T) {
	r := NewRegistry(time.Minute)
	base := Key{Rail: "nmi", AccountID: "1", Endpoint: "e", Credential: Fingerprint("k")}
	require.True(t, r.Verify(context.Background(), base, fixed(Simulated, nil)).Armed())
	_, seen := r.Lookup(base)
	require.True(t, seen)
	for _, mut := range []func(*Key){
		func(k *Key) { k.AccountID = "2" },
		func(k *Key) { k.Endpoint = "f" },
		func(k *Key) { k.Credential = Fingerprint("rotated") },
		func(k *Key) { k.Rail = "stripe" },
		func(k *Key) { k.Expect = Live },
	} {
		other := base
		mut(&other)
		_, seen := r.Lookup(other)
		require.False(t, seen, "%+v", other)
	}
	require.NotEqual(t, Fingerprint("ab", "c"), Fingerprint("a", "bc"), "parts are length-framed")
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
	ctx := context.Background()
	k := Key{Rail: "nmi", Credential: Fingerprint("c")}
	var tracked Tracked
	r.Verify(ctx, k, check)
	tracked.Add(k, check)
	tracked.Add(Key{Rail: "stripe", Credential: Fingerprint("d")}, fixed(Simulated, nil))
	require.Len(t, tracked.Disarmed(ctx, r), 1)
	up = true
	require.Len(t, tracked.Disarmed(ctx, r), 1, "still inside the retry backoff")
	now = now.Add(time.Minute)
	require.Empty(t, tracked.Disarmed(ctx, r), "a due retry re-arms without a restart")
}
