package checkout

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNMIScheduleBoundaryPreservesProviderPrecision(t *testing.T) {
	accepted := time.Date(2026, 10, 18, 12, 30, 0, 0, time.UTC)
	for _, row := range []struct {
		raw     string
		matches bool
	}{
		{"2026-10-18", true},
		{"2026-10-19", false},
		{"2026-10-18T12:30:00Z", true},
		{"2026-10-18T12:30:00", true},
		{"2026-10-18T00:00:00Z", false},
		{"2026-10-18T06:30:00-06:00", true},
		{"unreadable", false},
	} {
		t.Run(row.raw, func(t *testing.T) { require.Equal(t, row.matches, nmiScheduleBoundaryMatches(row.raw, accepted)) })
	}
}
