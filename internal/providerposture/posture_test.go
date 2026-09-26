package providerposture

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
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
		s := NewRegistry(func(int) time.Duration { return time.Minute }).Verify(context.Background(), k, tc.check)
		require.Equal(t, tc.armed, s.Armed(), "expect %s got %s", tc.expect, s.Verdict)
		if tc.armed {
			require.NoError(t, s.Error())
		} else {
			require.ErrorIs(t, s.Error(), ErrDisarmed)
		}
	}
	s := NewRegistry(func(int) time.Duration { return time.Minute }).Verify(context.Background(), Key{}, fixed(Unknown, blip))
	require.ErrorIs(t, s.Error(), blip, "the provider's reason is kept")
}

func TestRegistryVerifiesOnceAndRetriesOnlyUnknownAfterBackoff(t *testing.T) {
	now := time.Unix(0, 0)
	r := NewRegistry(func(int) time.Duration { return time.Minute })
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
	r := NewRegistry(func(int) time.Duration { return time.Minute })
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
	r := NewRegistry(func(int) time.Duration { return time.Minute })
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

func TestTrackedReportsCachedStateAndRecoversAfterBackoff(t *testing.T) {
	now := time.Unix(0, 0)
	var attempts []int
	r := NewRegistry(func(n int) time.Duration { attempts = append(attempts, n); return time.Minute })
	r.Now = func() time.Time { return now }
	up := false
	var calls atomic.Int64
	check := func(context.Context) (Verdict, error) {
		calls.Add(1)
		if up {
			return Simulated, nil
		}
		return Unknown, errors.New("blip")
	}
	ctx := context.Background()
	k := Key{Rail: "nmi", Credential: Fingerprint("c")}
	var tracked Tracked
	pspID := uuid.New()
	r.Verify(ctx, k, check)
	tracked.AddPSP(pspID, k, check)
	tracked.Add(Key{Rail: "stripe", Credential: Fingerprint("d")}, fixed(Simulated, nil))
	require.Len(t, tracked.Unarmed(r), 1, "an unverified key is not reported; the unknown one is")
	require.EqualValues(t, 1, calls.Load(), "reading state never probes")
	status, ok := tracked.PSPStatus(r, pspID)
	require.True(t, ok)
	require.Equal(t, Unknown, status.Verdict)

	up = true
	require.Equal(t, Unknown, r.Refresh(ctx, k, check).Verdict, "still inside the retry backoff")
	now = now.Add(time.Minute)
	require.True(t, r.Refresh(ctx, k, check).Armed(), "a due retry re-arms without a restart")
	require.Empty(t, tracked.Unarmed(r))
	require.Equal(t, []int{0}, attempts)

	up = false
	k2 := Key{Rail: "nmi", Credential: Fingerprint("e")}
	for range 3 {
		r.Verify(ctx, k2, check)
	}
	require.Equal(t, []int{0, 0, 1, 2}, attempts, "consecutive unknowns back off further")
}

// A provider that never answers cannot hold checkouts: concurrent callers
// share one bounded check and each returns within its own context.
func TestRequireIsSingleFlightBoundedAndContextAware(t *testing.T) {
	r := NewRegistry(func(int) time.Duration { return time.Minute })
	r.Timeout = time.Hour
	release := make(chan struct{})
	var calls atomic.Int64
	hang := func(context.Context) (Verdict, error) {
		calls.Add(1)
		<-release
		return Simulated, nil
	}
	k := Key{Rail: "nmi", Credential: Fingerprint("hang")}
	var wg sync.WaitGroup
	start := time.Now()
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			require.ErrorIs(t, r.Require(ctx, k, hang), ErrDisarmed)
		}()
	}
	wg.Wait()
	require.Less(t, time.Since(start), 2*time.Second, "callers return within their contexts")
	close(release)
	require.Eventually(t, func() bool { return r.Require(context.Background(), k, hang) == nil }, 2*time.Second, 10*time.Millisecond)
	require.EqualValues(t, 1, calls.Load(), "one check for every concurrent caller")

	r.Timeout = 50 * time.Millisecond
	slow := func(ctx context.Context) (Verdict, error) { <-ctx.Done(); return Unknown, ctx.Err() }
	start = time.Now()
	require.ErrorIs(t, r.Require(context.Background(), Key{Rail: "stripe", Credential: Fingerprint("slow")}, slow), ErrDisarmed)
	require.Less(t, time.Since(start), time.Second, "a check is bounded by the posture timeout")
}
