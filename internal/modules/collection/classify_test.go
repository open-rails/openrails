package collection

import (
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/decline"
)

// A soft decline keeps the policy's schedule unless the networks' advice
// forbids charging the card again.
func TestFailureActionReadsAdviceAndPolicy(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	policy := Policy{Tiers: []Tier{{Offsets: []time.Duration{12 * time.Hour}}}}
	soft := decline.Evidence{Rail: "stripe", Code: "insufficient_funds"}
	action, err := FailureAction(policy, 720, soft, 0, nil, now)
	if err != nil || action.NextAttemptAt == nil || !action.NextAttemptAt.Equal(now.Add(12*time.Hour)) {
		t.Fatalf("soft decline: %+v, %v; want a retry 12h on", action, err)
	}
	for _, advice := range []decline.Evidence{
		{Rail: "stripe", Code: "insufficient_funds", Advice: "do_not_try_again"},
		{Rail: "stripe", Code: "insufficient_funds", NetworkAdvice: "03"},
		{Rail: "stripe", Code: "insufficient_funds", IssuerCode: "14"},
	} {
		action, err := FailureAction(policy, 720, advice, 0, nil, now)
		if err != nil || !action.AwaitingPaymentMethod() || action.NextAttemptAt != nil || action.Terminal {
			t.Fatalf("%+v: %+v, %v; want a wait for a new card", advice, action, err)
		}
	}
}
