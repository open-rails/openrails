package providerposture

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A lasting verdict is proven once: it neither expires nor yields to a check
// the provider did not answer.
func TestALastingVerdictStandsUntilItsKeyChanges(t *testing.T) {
	r := NewRegistry(func(int) time.Duration { return 0 })
	var now atomic.Int64
	now.Store(time.Now().UnixNano())
	r.SetClock(func() time.Time { return time.Unix(0, now.Load()) })
	credential := Fingerprint("lasting")
	k := Key{Rail: "stripe", Endpoint: "https://api.example", AccountID: "acct_lasting", Credential: credential, Lasting: true}
	var checks atomic.Int32
	var answer atomic.Uint32
	answer.Store(uint32(Simulated))
	check := func(context.Context) (Verdict, error) {
		checks.Add(1)
		return Verdict(answer.Load()), nil
	}
	ctx := context.Background()
	r.Register(k, check)
	require.NoError(t, r.RequireCredential(ctx, k.Rail, k.Endpoint, credential))

	now.Add(int64(2 * VerdictTTL))
	require.NoError(t, r.RequireCredential(ctx, k.Rail, k.Endpoint, credential))
	require.EqualValues(t, 1, checks.Load(), "a lasting verdict is not proven again")

	answer.Store(uint32(Unknown))
	status := r.Verify(ctx, k, check)
	require.True(t, status.Armed(), "an unanswered check never undoes it")
	require.NoError(t, r.RequireCredential(ctx, k.Rail, k.Endpoint, credential))
}
