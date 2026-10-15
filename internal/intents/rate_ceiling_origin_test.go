package intents

import (
	"context"
	"testing"
	"time"
)

func TestRateCeilingRefusesUnknownOriginForDestructiveOps(t *testing.T) {
	for _, typ := range DestructiveIntentTypes() {
		err := (&RateCeiling{}).Check(context.Background(), CheckParams{IntentType: typ, Origin: "bogus"}, time.Now())
		if err == nil {
			t.Fatalf("%s with unknown origin passed the ceiling", typ)
		}
	}
}

// Their customer request already reserved the combined merchant budget. A
// delayed provider cancellation must not add a per-actor or hourly quota.
func TestCustomerActionsDoNotSpendLegacyHourlyCeiling(t *testing.T) {
	for _, typ := range []string{TypeNMIPaymentMethodDelete, TypeHyperSwitchMethodDelete, TypePlatformMethodDelete, TypeNMIDeleteSubscription, TypeCCBillCancelSubscription, TypeStripeCancelSubscription} {
		if err := (&RateCeiling{}).Check(context.Background(), CheckParams{Origin: OriginUser, IntentType: typ}, time.Now()); err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		if err := (&RateCeiling{}).Check(context.Background(), CheckParams{Origin: OriginAdmin, IntentType: TypeNMIPaymentMethodDelete}, time.Now()); err == nil {
			t.Fatal("admin ceiling unexpectedly bypassed")
		}
	}
}
