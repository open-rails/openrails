package reconcile

import "testing"

// or#858: `SubscriptionsExhaustive` used to be a bare bool stamped straight onto
// Coverage. It is an ABSENCE PROOF — every local subscription the batch omits is
// cancelled — so an importer that batches its book and forgets to clear the flag
// cancels everything it did not happen to send, silently. A boolean cannot tell
// a full book from a partial one; a count can.
func TestDeclaredCoverageRefusesUnconfirmedExhaustiveBook(t *testing.T) {
	n := func(i int) *int { return &i }
	for _, tc := range []struct {
		name    string
		cov     DeclaredCoverage
		facts   int
		wantErr string
	}{
		{"non-exhaustive needs no confirmation", DeclaredCoverage{}, 3, ""},
		{"non-exhaustive empty batch is fine", DeclaredCoverage{}, 0, ""},
		{"exhaustive without a count refuses", DeclaredCoverage{SubscriptionsExhaustive: true}, 3, "must declare expected_subscriptions"},
		{"exhaustive with the wrong count refuses", DeclaredCoverage{SubscriptionsExhaustive: true, ExpectedSubscriptions: n(40)}, 3, "says 40 but the call carries 3"},
		{"exhaustive with zero subscriptions refuses", DeclaredCoverage{SubscriptionsExhaustive: true, ExpectedSubscriptions: n(0)}, 0, "zero subscriptions"},
		{"exhaustive with a matching count is accepted", DeclaredCoverage{SubscriptionsExhaustive: true, ExpectedSubscriptions: n(3)}, 3, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cov.validate(tc.facts)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("want refusal containing %q, got nil", tc.wantErr)
			case tc.wantErr != "" && !contains(err.Error(), tc.wantErr):
				t.Fatalf("want refusal containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
