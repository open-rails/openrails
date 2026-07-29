package nmi

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #348 probe-cooldown policy: only fresh, conclusive verdicts short-circuit
// the probe; everything else (stale, unknown, clock weirdness) probes.
func TestProbeCacheDecision(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		verdict   string
		checkedAt time.Time
		want      ProbeCacheAction
	}{
		{"fresh live refuses from cache", ProbeVerdictLive, now.Add(-1 * time.Hour), ProbeCacheRefuseBoot},
		{"fresh simulated skips the probe", ProbeVerdictSimulated, now.Add(-11 * time.Hour), ProbeCacheSkipProbe},
		{"live verdict exactly at the cooldown re-probes", ProbeVerdictLive, now.Add(-ProbeVerdictCooldown), ProbeCacheMiss},
		{"stale live re-probes", ProbeVerdictLive, now.Add(-13 * time.Hour), ProbeCacheMiss},
		{"stale simulated re-probes", ProbeVerdictSimulated, now.Add(-48 * time.Hour), ProbeCacheMiss},
		{"unknown verdict value re-probes", "indeterminate", now.Add(-time.Hour), ProbeCacheMiss},
		{"future checked_at (clock skew) re-probes", ProbeVerdictLive, now.Add(time.Hour), ProbeCacheMiss},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ProbeCacheDecision(tc.verdict, tc.checkedAt, now))
		})
	}
}

func TestProbeKeyHash(t *testing.T) {
	a := ProbeKeyHash("security-key-A")
	b := ProbeKeyHash("security-key-B")
	assert.NotEqual(t, a, b, "a rotated key must hash to a different cache identity")
	assert.Equal(t, a, ProbeKeyHash("security-key-A"), "deterministic")
	assert.Len(t, a, 64, "sha256 hex")
	assert.NotContains(t, a, "security-key", "the key itself is never stored")
}

// TestCheckTestModeArmWithoutCache exercises the live-probe leg of
// CheckTestModeArm with a nil query catalog (cache degraded/unavailable):
// every call must re-probe, and only a declined test-card auth (a live
// account) refuses — an approved probe or a gateway error must not.
func TestCheckTestModeArmWithoutCache(t *testing.T) {
	t.Run("simulated account never refuses", func(t *testing.T) {
		server := probeServer(t, "1", nil)
		defer server.Close()
		decision := CheckTestModeArm(context.Background(), nil, probeClient(t, server.URL), "cache-key")
		require.False(t, decision.Refuse)
		require.False(t, decision.Cached)
		require.NoError(t, decision.ProbeErr)
	})

	t.Run("live account refuses", func(t *testing.T) {
		server := probeServer(t, "2", nil)
		defer server.Close()
		decision := CheckTestModeArm(context.Background(), nil, probeClient(t, server.URL), "cache-key")
		require.True(t, decision.Refuse)
		require.False(t, decision.Cached)
	})

	t.Run("indeterminate probe never refuses", func(t *testing.T) {
		server := probeServer(t, "3", nil)
		defer server.Close()
		decision := CheckTestModeArm(context.Background(), nil, probeClient(t, server.URL), "cache-key")
		require.False(t, decision.Refuse)
		require.Error(t, decision.ProbeErr)
	})
}
