package money

import (
	"testing"
	"time"
)

func TestInvoiceCollectionAction(t *testing.T) {
	t.Parallel()

	first := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	insufficientFunds := "insufficient_funds"
	expiredCard := "expired_card"

	tests := []struct {
		name            string
		code            *string
		currentFailures int32
		firstFailureAt  *time.Time
		wantNext        *time.Time
		wantTerminal    bool
	}{
		{
			name:     "first recoverable failure retries on day two",
			code:     &insufficientFunds,
			wantNext: timePointer(first.Add(2 * 24 * time.Hour)),
		},
		{
			name:            "later retry remains anchored to first failure",
			code:            &insufficientFunds,
			currentFailures: 2,
			firstFailureAt:  &first,
			wantNext:        timePointer(first.Add(9 * 24 * time.Hour)),
		},
		{
			name:            "fifth recoverable failure is terminal",
			code:            &insufficientFunds,
			currentFailures: 4,
			firstFailureAt:  &first,
			wantTerminal:    true,
		},
		{
			name: "card replacement failure pauses retries",
			code: &expiredCard,
		},
		{
			name: "unknown failure pauses retries",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			action := invoiceCollectionAction("nmi", test.code, test.currentFailures, test.firstFailureAt, first)
			if action.terminal != test.wantTerminal {
				t.Fatalf("terminal = %v, want %v", action.terminal, test.wantTerminal)
			}
			if !equalTimePointers(action.nextAttemptAt, test.wantNext) {
				t.Fatalf("next attempt = %v, want %v", action.nextAttemptAt, test.wantNext)
			}
		})
	}
}

func timePointer(value time.Time) *time.Time { return &value }

func equalTimePointers(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.Equal(*right)
}
