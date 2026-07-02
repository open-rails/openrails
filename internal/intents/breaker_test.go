package intents

import "testing"

// #679 breaker budget: max(floor, pct% of active subscriptions).
func TestDestructiveBudget(t *testing.T) {
	cases := []struct {
		active int64
		want   int64
	}{
		{0, 25},     // floor for empty merchants
		{100, 25},   // 1% = 1 < floor
		{2_499, 25}, // 1% = 24 < floor
		{2_500, 25}, // 1% = exactly the floor
		{2_600, 26},
		{100_000, 1_000},
	}
	for _, c := range cases {
		if got := DestructiveBudget(c.active); got != c.want {
			t.Errorf("DestructiveBudget(%d) = %d, want %d", c.active, got, c.want)
		}
	}
}

func TestIsDestructiveIntentType(t *testing.T) {
	if !IsDestructiveIntentType(TypeNMIDeleteSubscription) {
		t.Fatal("nmi_delete_subscription must be destructive")
	}
	if !IsDestructiveIntentType(TypeCCBillCancelSubscription) {
		t.Fatal("ccbill_cancel_subscription must be destructive (#696: no resume API)")
	}
	if !IsDestructiveIntentType(TypeNMIVaultDelete) {
		t.Fatal("nmi_vault_delete must be destructive (#674 tail: stored card is irrecoverable)")
	}
	if IsDestructiveIntentType(TypeManualRebill) {
		t.Fatal("manual_rebill must not be destructive (charges are not breaker-gated)")
	}
	types := DestructiveIntentTypes()
	if len(types) != 3 || types[0] != TypeCCBillCancelSubscription || types[1] != TypeNMIDeleteSubscription || types[2] != TypeNMIVaultDelete {
		t.Fatalf("DestructiveIntentTypes() = %v", types)
	}
}
