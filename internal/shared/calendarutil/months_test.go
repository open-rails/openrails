package calendarutil

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCalendarBoundaryPreservesAnchor(t *testing.T) {
	for _, test := range []struct {
		start  string
		months int
		want   []string
	}{
		{"2025-01-15T12:34:56Z", 1, []string{"2025-02-15T12:34:56Z", "2025-03-15T12:34:56Z"}},
		{"2024-02-29T12:34:56Z", 12, []string{"2025-02-28T12:34:56Z", "2026-02-28T12:34:56Z", "2027-02-28T12:34:56Z", "2028-02-29T12:34:56Z"}},
		{"2025-01-31T12:34:56Z", 1, []string{"2025-02-28T12:34:56Z", "2025-03-31T12:34:56Z", "2025-04-30T12:34:56Z"}},
		{"2024-01-31T12:34:56Z", 1, []string{"2024-02-29T12:34:56Z", "2024-03-31T12:34:56Z"}},
		{"2025-01-30T12:34:56Z", 1, []string{"2025-02-28T12:34:56Z", "2025-03-30T12:34:56Z"}},
		{"2025-02-28T12:34:56Z", 1, []string{"2025-03-28T12:34:56Z", "2025-04-28T12:34:56Z"}},
		{"2024-12-31T12:34:56Z", 2, []string{"2025-02-28T12:34:56Z", "2025-04-30T12:34:56Z", "2025-06-30T12:34:56Z"}},
	} {
		t.Run(test.start, func(t *testing.T) {
			anchor, err := time.Parse(time.RFC3339, test.start)
			require.NoError(t, err)
			boundary := anchor
			for _, expected := range test.want {
				boundary, err = NextBoundary(anchor.In(time.FixedZone("offset", -7*3600)), boundary, test.months)
				require.NoError(t, err)
				require.Equal(t, expected, boundary.Format(time.RFC3339))
			}
		})
	}
	anchor := time.Date(2025, 1, 31, 0, 0, 0, 0, time.UTC)
	_, err := NextBoundary(anchor, anchor.Add(30*24*time.Hour), 1)
	require.Error(t, err, "a fixed 30-day boundary is not a monthly boundary")
	_, err = NextBoundary(anchor, anchor, 0)
	require.Error(t, err)
	_, err = NextBoundary(time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC), time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC), 1)
	require.Error(t, err)
}
