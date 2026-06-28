package recurring

import (
	"errors"
	"testing"

	"github.com/open-rails/openrails/internal/billing/declinecode"
)

func TestClassifyCrankError(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode declinecode.Code
		wantCat  declinecode.Category
		wantOn   int
	}{
		{"nil", nil, "", "", -1},
		{"cap reached json (already paid)", errors.New(`{"InstructionError":[0,{"Custom":400}]}`), declinecode.DuplicateTransaction, declinecode.AlreadyPaid, 400},
		{"cap reached hex", errors.New("transaction failed: custom program error: 0x190"), declinecode.DuplicateTransaction, declinecode.AlreadyPaid, 400},
		{"delegate revoked (terminal)", errors.New(`{"InstructionError":[0,{"Custom":4}]}`), declinecode.DeclinedStopRecurring, declinecode.Terminal, 4},
		{"insufficient funds (dun)", errors.New(`{"InstructionError":[0,{"Custom":1}]}`), declinecode.InsufficientFunds, declinecode.Recoverable, 1},
		{"subscription cancelled at period end (terminal)", errors.New(`{"InstructionError":[0,{"Custom":508}]}`), declinecode.DeclinedStopRecurring, declinecode.Terminal, 508},
		{"plan terms mismatch / ghost plan (terminal)", errors.New(`{"InstructionError":[0,{"Custom":519}]}`), declinecode.DeclinedStopRecurring, declinecode.Terminal, 519},
		{"rpc timeout (operational)", errors.New("rpc error: context deadline exceeded"), declinecode.CommunicationError, declinecode.Operational, -1},
		{"fee payer out of sol (operational)", errors.New("Transaction simulation failed: Insufficient funds for fee"), declinecode.CommunicationError, declinecode.Operational, -1},
		{"unknown custom code (conservative dun)", errors.New(`{"InstructionError":[0,{"Custom":6001}]}`), declinecode.GenericDecline, declinecode.Recoverable, 6001},
		{"unknown non-coded (conservative dun)", errors.New("something weird"), declinecode.GenericDecline, declinecode.Recoverable, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ClassifyCrankError(c.err)
			if got.Code != c.wantCode || got.Category != c.wantCat || got.OnChainCode != c.wantOn {
				t.Fatalf("ClassifyCrankError(%v) = {Code:%q Cat:%q On:%d}, want {Code:%q Cat:%q On:%d}",
					c.err, got.Code, got.Category, got.OnChainCode, c.wantCode, c.wantCat, c.wantOn)
			}
		})
	}
}
