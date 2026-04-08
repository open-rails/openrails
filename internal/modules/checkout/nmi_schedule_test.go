package checkout

import (
	"testing"
	"time"
)

func TestBuildNMIFutureStartDate(t *testing.T) {
	now := time.Date(2026, time.April, 8, 10, 30, 0, 0, time.UTC)

	tests := []struct {
		name         string
		target       time.Time
		expectedDate string
		expectedTime time.Time
	}{
		{
			name:         "same day bumps to tomorrow",
			target:       time.Date(2026, time.April, 8, 23, 59, 0, 0, time.UTC),
			expectedDate: "20260409",
			expectedTime: time.Date(2026, time.April, 9, 0, 0, 0, 0, time.UTC),
		},
		{
			name:         "past day bumps to tomorrow",
			target:       time.Date(2026, time.April, 7, 23, 59, 0, 0, time.UTC),
			expectedDate: "20260409",
			expectedTime: time.Date(2026, time.April, 9, 0, 0, 0, 0, time.UTC),
		},
		{
			name:         "future day preserved",
			target:       time.Date(2026, time.April, 12, 14, 0, 0, 0, time.UTC),
			expectedDate: "20260412",
			expectedTime: time.Date(2026, time.April, 12, 0, 0, 0, 0, time.UTC),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			formatted, startAt := buildNMIFutureStartDate(tt.target, now)
			if formatted != tt.expectedDate {
				t.Fatalf("expected %s, got %s", tt.expectedDate, formatted)
			}
			if !startAt.Equal(tt.expectedTime) {
				t.Fatalf("expected %v, got %v", tt.expectedTime, startAt)
			}
		})
	}
}
