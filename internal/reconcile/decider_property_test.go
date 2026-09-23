package reconcile

import (
	"testing"
	"time"
)

func TestDecide_EvidencelessBundleCanOnlyPark(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	times := []*time.Time{nil}
	for _, d := range []time.Duration{-90 * 24 * time.Hour, -10 * 24 * time.Hour, -time.Hour, 20 * 24 * time.Hour} {
		tt := now.Add(d)
		times = append(times, &tt)
	}
	empty := EvidenceBundle{}
	for _, status := range []string{"active", "past_due", "pending", "unknown", "cancelled", "expired", "failed"} {
		for _, rail := range []string{"nmi", "ccbill", "stripe", "solana"} {
			for _, vaulted := range []bool{true, false} {
				for _, pe := range times {
					for _, grace := range times {
						for _, retry := range []bool{true, false} {
							sub := SubscriptionState{
								Status: status, Rail: rail, HasPaymentMethod: vaulted,
								RailSubscriptionID: "rs", PeriodEnd: pe,
								GraceEndsAt: grace, NextRetryScheduled: retry,
							}
							d := Decide(sub, empty, now, 0)
							if d.Kind != TransitionNone && d.Kind != TransitionParkUnknown {
								t.Fatalf("evidence-less bundle produced %v for state %+v (#664: no evidence, no action)", d.Kind, sub)
							}
						}
					}
				}
			}
		}
	}
}
