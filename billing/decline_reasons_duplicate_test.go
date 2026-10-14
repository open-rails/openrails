package billing

import "testing"

// A duplicate refusal tells the buyer nothing was charged twice, not "try again".
func TestDuplicateRefusalHasItsOwnMessage(t *testing.T) {
	f := DeclineDuplicateTransaction.Failure()
	if f.Reason != "duplicate_transaction" || f.Message == DeclineProcessingError.Failure().Message {
		t.Fatalf("duplicate refusal reads %+v", f)
	}
}
