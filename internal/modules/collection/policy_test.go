package collection

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDefaultPolicyIsTheBuiltInSchedule(t *testing.T) {
	t.Parallel()
	require.NoError(t, DefaultPolicy.Validate())
	for _, hours := range []int{1, 24, 95, 96, 168, 671, 672, 720, 8760} {
		want, _ := RetryOffsets(hours)
		got, err := DefaultPolicy.RetryOffsets(hours)
		require.NoError(t, err)
		require.Equal(t, want, got, "%dh", hours)
	}
}

func TestPolicyValidate(t *testing.T) {
	t.Parallel()
	day := 24 * time.Hour
	for _, tc := range []struct {
		name string
		p    Policy
		ok   bool
	}{
		{"one open tier", Policy{Tiers: []Tier{{Offsets: []time.Duration{day, 3 * day}}}}, false},
		{"weekly and monthly", Policy{Tiers: []Tier{{MaxCycle: 4 * day}, {MaxCycle: 28 * day, Offsets: []time.Duration{day}}, {Offsets: []time.Duration{day, 3 * day, 7 * day}}}}, true},
		{"retries past the cycle", Policy{Tiers: []Tier{{MaxCycle: 28 * day, Offsets: []time.Duration{day, 8 * day}}, {Offsets: []time.Duration{day}}}}, false},
		{"decreasing offsets", Policy{Tiers: []Tier{{MaxCycle: 4 * day}, {Offsets: []time.Duration{3 * day, day}}}}, false},
		{"open tier not last", Policy{Tiers: []Tier{{}, {MaxCycle: 4 * day}}}, false},
		{"no tiers", Policy{}, false},
		{"long transient", Policy{Tiers: []Tier{{MaxCycle: 4 * day}, {Offsets: []time.Duration{day}}}, Transient: []time.Duration{3 * time.Hour}}, false},
	} {
		err := tc.p.Validate()
		if tc.ok {
			require.NoError(t, err, tc.name)
		} else {
			require.Error(t, err, tc.name)
		}
	}
}

func TestPolicySchedule(t *testing.T) {
	t.Parallel()
	day := 24 * time.Hour
	p := Policy{Tiers: []Tier{{MaxCycle: 4 * day}, {MaxCycle: 28 * day, Offsets: []time.Duration{day}}, {Offsets: []time.Duration{day, 3 * day, 7 * day}}}, Transient: []time.Duration{10 * time.Minute}}
	require.NoError(t, p.Validate())
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	max, err := p.MaxFailures(720)
	require.NoError(t, err)
	require.Equal(t, 4, max)
	next, ok, err := p.NextAttemptAt(720, 2, at)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, at.Add(2*day), next, "gaps follow offsets from the first decline")
	_, ok, _ = p.NextAttemptAt(720, 4, at)
	require.False(t, ok)
	w, err := p.Window(720)
	require.NoError(t, err)
	require.Equal(t, 8*day, w)
	nt, ok := p.NextTransientAttempt(0, at)
	require.True(t, ok)
	require.Equal(t, at.Add(10*time.Minute), nt)
	_, ok = p.NextTransientAttempt(1, at)
	require.False(t, ok)
}
