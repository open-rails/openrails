package cardguard

import "testing"

// An E.164 phone number is one unseparated run of at most 15 digits after a
// +; a + before a longer or separated card number hides nothing.
func TestAPlusExemptsOnlyAnE164Number(t *testing.T) {
	for value, card := range map[string]bool{
		"+4915112345678":       false, // 13 digits, Luhn-valid: a German mobile
		"+491511234567891":     false, // 15 digits, Luhn-valid: E.164's longest
		"+4111111111111111":    true,  // 16 digits
		"+4111 1111 1111 1111": true,  // separated
		"4915112345678":        true,  // no +: as long as a card number
	} {
		if got := ContainsPAN(value); got != card {
			t.Errorf("ContainsPAN(%q) = %v, want %v", value, got, card)
		}
	}
}
