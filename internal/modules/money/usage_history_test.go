package money

import (
	"slices"
	"testing"
)

func TestLargestRemainder(t *testing.T) {
	for _, c := range []struct {
		amount  int64
		weights []int64
		want    []int64
	}{
		{8, []int64{20, 8}, []int64{6, 2}},
		{8, []int64{8, 20}, []int64{2, 6}},
		{10, []int64{1, 1, 1}, []int64{4, 3, 3}},
		{0, []int64{3, 4}, []int64{0, 0}},
		{7, []int64{5}, []int64{7}},
	} {
		if got := largestRemainder(c.amount, c.weights); !slices.Equal(got, c.want) {
			t.Errorf("largestRemainder(%d, %v) = %v, want %v", c.amount, c.weights, got, c.want)
		}
	}
}
