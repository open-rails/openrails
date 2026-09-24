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
