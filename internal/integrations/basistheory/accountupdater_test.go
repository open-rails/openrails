package basistheory

import "testing"

// or#795: the result vocabulary maps onto the actions that already exist, and
// nothing else. A code this build does not know is UNRECOGNIZED — recorded
// verbatim by the caller and folded by nothing — because guessing on a money
// path is worse than a gap an operator can see.
func TestClassifyAccountUpdaterResult(t *testing.T) {
	for code, want := range map[string]AUOutcome{
		AUUpdatedPAN:        AUOutcomeUpdated,
		AUUpdatedExpDate:    AUOutcomeUpdated,
		"upd_pan":           AUOutcomeUpdated, // case is the wire's business
		"UPD_PAN_EXP_DATE":  AUOutcomeUpdated, // the family, not a fixed list
		AUNoUpdate:          AUOutcomeNoChange,
		AUNoMatch:           AUOutcomeNoChange,
		"":                  AUOutcomeNoChange,
		AUClosedAccount:     AUOutcomeClosed,
		AUContactCardholder: AUOutcomeContactCardholder,
		"WRN_SOMETHING_NEW": AUOutcomeUnrecognized,
		"nonsense":          AUOutcomeUnrecognized,
	} {
		if got := ClassifyAccountUpdaterResult(code); got != want {
			t.Errorf("Classify(%q) = %q, want %q", code, got, want)
		}
	}
}
