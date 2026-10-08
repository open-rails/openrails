package intents

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStripeReplayStopsBeforeKeyRetentionExpires(t *testing.T) {
	first := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	history := SubmissionHistory{First: first, Latest: first.Add(23 * time.Hour), Resends: 1}
	limit := first.Add(24*time.Hour - ClockMargin - ProviderCallHold)
	require.False(t, (SubmissionHistory{}).StripeReplaySafe(first))
	require.False(t, history.StripeReplaySafe(first.Add(-time.Second)), "clock rollback is not a longer retention window")
	require.True(t, history.StripeReplaySafe(limit.Add(-time.Nanosecond)))
	require.False(t, history.StripeReplaySafe(limit), "reserve clock skew and the complete provider-call hold")
	require.False(t, history.StripeReplaySafe(first.Add(24*time.Hour)), "a recent resend never resets the original window")
}
