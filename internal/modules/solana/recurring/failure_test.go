package recurring

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestIsOperationalFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context deadline", context.DeadlineExceeded, true},
		{"context canceled", context.Canceled, true},
		{"wrapped deadline", fmt.Errorf("submit: %w", context.DeadlineExceeded), true},
		{"rpc timeout", errors.New("rpc error: context deadline exceeded waiting for confirmation"), true},
		{"connection refused", errors.New("dial tcp: connection refused"), true},
		{"429 rate limit", errors.New("HTTP 429: too many requests"), true},
		{"service unavailable", errors.New("503 service unavailable"), true},
		{"not confirmed", errors.New("transaction was not confirmed in 30s"), true},
		{"blockhash not found", errors.New("Blockhash not found"), true},
		{"fee payer out of sol", errors.New("Transaction simulation failed: Insufficient funds for fee"), true},
		{"no prior credit (fee payer)", errors.New("Attempt to debit an account but found no record of a prior credit"), true},
		// Subscriber faults must NOT be classified operational.
		{"insufficient usdc", errors.New("Transfer: insufficient funds"), false},
		{"custom program error 0x1", errors.New("custom program error: 0x1"), false},
		{"revoked delegation", errors.New("delegation has been revoked"), false},
		{"plan terms mismatch", errors.New("PlanTermsMismatch"), false},
		{"unknown", errors.New("something weird happened"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsOperationalFailure(c.err); got != c.want {
				t.Fatalf("IsOperationalFailure(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
