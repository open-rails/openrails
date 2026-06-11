package app

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// #348 probe-cooldown policy: only fresh, conclusive verdicts short-circuit
// the boot probe; everything else (stale, unknown, clock weirdness) probes.
func TestProbeCacheDecision(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		verdict   string
		checkedAt time.Time
		want      probeCacheAction
	}{
		{"fresh live refuses boot from cache", probeVerdictLive, now.Add(-1 * time.Hour), probeCacheRefuseBoot},
		{"fresh simulated skips the probe", probeVerdictSimulated, now.Add(-11 * time.Hour), probeCacheSkipProbe},
		{"live verdict exactly at the cooldown re-probes", probeVerdictLive, now.Add(-probeVerdictCooldown), probeCacheMiss},
		{"stale live re-probes", probeVerdictLive, now.Add(-13 * time.Hour), probeCacheMiss},
		{"stale simulated re-probes", probeVerdictSimulated, now.Add(-48 * time.Hour), probeCacheMiss},
		{"unknown verdict value re-probes", "indeterminate", now.Add(-time.Hour), probeCacheMiss},
		{"future checked_at (clock skew) re-probes", probeVerdictLive, now.Add(time.Hour), probeCacheMiss},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, probeCacheDecision(tc.verdict, tc.checkedAt, now))
		})
	}
}

func TestProbeKeyHash(t *testing.T) {
	a := probeKeyHash("security-key-A")
	b := probeKeyHash("security-key-B")
	assert.NotEqual(t, a, b, "a rotated key must hash to a different cache identity")
	assert.Equal(t, a, probeKeyHash("security-key-A"), "deterministic")
	assert.Len(t, a, 64, "sha256 hex")
	assert.NotContains(t, a, "security-key", "the key itself is never stored")
}
