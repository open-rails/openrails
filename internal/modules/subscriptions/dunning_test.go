package subscriptions

import "testing"

func TestClassifyNMIDecline(t *testing.T) {
	hard := []int{201, 204, 220, 221, 222, 223, 224, 225, 226, 240, 250, 251, 252, 253, 261, 262, 263, 461}
	for _, code := range hard {
		if got := ClassifyNMIDecline(code); got != DeclineHard {
			t.Errorf("code %d: expected DeclineHard, got %v", code, got)
		}
	}

	soft := []int{0, 100, 200, 202, 203, 260, 264, 300, 400, 410, 411, 420, 421, 430, 440, 441, 460, 999}
	for _, code := range soft {
		if got := ClassifyNMIDecline(code); got != DeclineSoft {
			t.Errorf("code %d: expected DeclineSoft, got %v", code, got)
		}
	}
}
