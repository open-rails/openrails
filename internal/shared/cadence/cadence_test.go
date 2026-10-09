package cadence

import (
	"testing"
	"time"
)

func TestFormatDuration(t *testing.T) {
	for _, tt := range []struct {
		in   time.Duration
		want string
	}{
		{720 * time.Hour, "30 days"},
		{168 * time.Hour, "1 week"},
		{8760 * time.Hour, "365 days"},
		{time.Hour, "1 hour"},
		{36 * time.Hour, "36 hours"},
		{90 * time.Minute, "90 minutes"},
		{45 * time.Second, "45 seconds"},
		{-2 * time.Hour, "-2 hours"},
		{0, "0 seconds"},
		{1500 * time.Millisecond, "1.5s"},
	} {
		if got := FormatDuration(tt.in); got != tt.want {
			t.Errorf("FormatDuration(%s) = %q, want %q", tt.in, got, tt.want)
		}
	}
	if got := FormatHours(72); got != "3 days" {
		t.Errorf("FormatHours(72) = %q", got)
	}
}
