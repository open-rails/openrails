package reconcile

import "testing"

func TestCancelBudget_Limit(t *testing.T) {
	var b CancelBudget
	cases := map[int]int{
		0:     3,  // small-book allowance: a tiny merchant must still converge
		9:     3,  // 5% of 9 rounds to 0 -> allowance; all nine would still trip
		20:    3,  // 5% of 20 = 1 -> allowance
		100:   5,  // 5%
		500:   25, // 5% would be 25, equal to the absolute cap
		1000:  25, // absolute cap wins over 5% (=50)
		10000: 25,
	}
	for localLive, want := range cases {
		if got := b.Limit(localLive); got != want {
			t.Errorf("Limit(%d) = %d, want %d", localLive, got, want)
		}
	}
}
