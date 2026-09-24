package reconcile

import (
	"math"
	"testing"
)

func TestParseAmountCentsRefusesOverflow(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want int64
		err  bool
	}{
		{in: "92233720368547758.07", want: math.MaxInt64},
		{in: "-92233720368547758.07", want: -math.MaxInt64},
		{in: "92233720368547758.08", err: true},
		{in: "999999999999999999999", err: true},
	} {
		got, err := parseAmountCents(tt.in)
		if (err != nil) != tt.err || got != tt.want {
			t.Errorf("parseAmountCents(%q) = %d, %v", tt.in, got, err)
		}
	}
}
