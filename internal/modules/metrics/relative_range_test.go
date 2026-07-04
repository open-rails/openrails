package metrics

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Relative ranges ({"last":"7d"}) resolve to inclusive UTC day windows ending
// today — the #741 saved-widget contract ("past 7 days" stays current).
func TestValidateAt_RelativeRange(t *testing.T) {
	now := time.Date(2026, 7, 3, 15, 30, 0, 0, time.UTC)

	q := &Query{Measures: []string{"cancellations"}, By: []string{"time"}, Grain: "day",
		Range: &QueryRange{Last: "7d"}}
	plan, verr := ValidateAt(q, now)
	require.Nil(t, verr)
	require.Equal(t, time.Date(2026, 6, 27, 0, 0, 0, 0, time.UTC), plan.From)
	require.Equal(t, time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC), plan.To, "to = end of today (exclusive bound)")
	require.Len(t, plan.Buckets, 7, "past 7 days = 7 day buckets incl today")

	for _, tc := range []struct {
		last string
		from time.Time
	}{
		{"12w", time.Date(2026, 4, 11, 0, 0, 0, 0, time.UTC)},
		{"6m", time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC)},
		{"1y", time.Date(2025, 7, 4, 0, 0, 0, 0, time.UTC)},
	} {
		plan, verr := ValidateAt(&Query{Measures: []string{"cancellations"}, Range: &QueryRange{Last: tc.last}}, now)
		require.Nil(t, verr, "last=%s", tc.last)
		require.Equal(t, tc.from, plan.From, "last=%s", tc.last)
	}
}

func TestValidateAt_RelativeRangeErrors(t *testing.T) {
	now := time.Now().UTC()

	// Malformed token → corrective error.
	_, verr := ValidateAt(&Query{Measures: []string{"cancellations"}, Range: &QueryRange{Last: "seven days"}}, now)
	require.NotNil(t, verr)
	require.Equal(t, "invalid_range", verr.Errors[0].Code)
	require.Equal(t, "range.last", verr.Errors[0].Param)

	// last + from/to are mutually exclusive.
	_, verr = ValidateAt(&Query{Measures: []string{"cancellations"},
		Range: &QueryRange{Last: "7d", From: "2026-01-01", To: "2026-02-01"}}, now)
	require.NotNil(t, verr)
	require.Contains(t, verr.Errors[0].Message, "mutually exclusive")

	// Zero-length window rejected.
	_, verr = ValidateAt(&Query{Measures: []string{"cancellations"}, Range: &QueryRange{Last: "0d"}}, now)
	require.NotNil(t, verr)
}
