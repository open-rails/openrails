package recurring

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// TestIsTerminalFailure exercises the third failure category (#265): the
// subscription is gone/inactive on-chain because the subscriber cancelled or
// revoked the delegation out-of-band. Terminal errors must cancel the
// subscription, not dun it.
func TestIsTerminalFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		// Terminal: subscription cancelled / revoked / inactive / PDA closed.
		{"cancelled", errors.New("subscription cancelled by owner"), true},
		{"canceled (us spelling)", errors.New("Subscription Canceled"), true},
		{"revoked", errors.New("delegation has been revoked"), true},
		{"delegation", errors.New("custom program error: DelegationNotFound"), true},
		{"not active", errors.New("subscription is not active"), true},
		{"inactive", errors.New("subscription inactive"), true},
		{"subscription closed", errors.New("Subscription closed"), true},
		{"account does not exist", errors.New("the subscription account does not exist"), true},
		{"accountnotfound", errors.New("Error: AccountNotFound for PDA"), true},
		{"wrapped terminal", fmt.Errorf("crank: %w", errors.New("delegation revoked")), true},

		// Operational must NOT be classified terminal — a transient RPC blip is
		// not a cancel.
		{"rpc timeout", errors.New("rpc error: context deadline exceeded"), false},
		{"connection refused", errors.New("dial tcp: connection refused"), false},
		{"fee payer out of sol", errors.New("Insufficient funds for fee"), false},
		{"context deadline", context.DeadlineExceeded, false},

		// Recoverable subscriber-fault must NOT be classified terminal — these
		// should still dun.
		{"insufficient usdc", errors.New("Transfer: insufficient funds"), false},
		{"custom program error 0x1", errors.New("custom program error: 0x1"), false},
		{"plan terms mismatch", errors.New("PlanTermsMismatch"), false},
		{"unknown", errors.New("something weird happened"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsTerminalFailure(c.err); got != c.want {
				t.Fatalf("IsTerminalFailure(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// TestFailureClassificationOrdering documents the required precedence:
// operational (retry) is checked before terminal (cancel), and a terminal
// string is never also operational. This guards the crankOne ordering in
// jobs_solana_crank.go.
func TestFailureClassificationOrdering(t *testing.T) {
	operational := errors.New("rpc error: connection reset by peer")
	if !IsOperationalFailure(operational) {
		t.Fatal("expected operational error to be operational")
	}
	if IsTerminalFailure(operational) {
		t.Fatal("operational error must not be classified terminal")
	}

	terminal := errors.New("subscription cancelled / delegation revoked")
	if IsOperationalFailure(terminal) {
		t.Fatal("terminal cancel must not be classified operational")
	}
	if !IsTerminalFailure(terminal) {
		t.Fatal("expected terminal cancel to be terminal")
	}
}
