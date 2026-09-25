package reconcile

import (
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
)

// The provider-owned snapshot law reasons in whole days (dunning windows,
// grace slack). Engine subscriptions, at any cadence, never reach it: they
// carry no provider subscription (subscriptions_engine_binding_check) and the
// first-party law leaves their cadence to the accepted operation.
func TestEngineSubscriptionsNeverReachSnapshotLaw(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	snap := &RemoteSnapshot{FetchedAt: now, Coverage: SnapshotCoverage{SubscriptionsExhaustive: true},
		Subscriptions: []RemoteSubscription{{RailSubscriptionID: "", Status: SubscriptionStatusCancelled}}}
	for _, cadence := range []time.Duration{time.Hour, 24 * time.Hour, 7 * 24 * time.Hour, 720 * time.Hour, 90 * 24 * time.Hour, 365 * 24 * time.Hour} {
		for _, lapsed := range []time.Duration{time.Minute, cadence / 2, cadence, 3 * cadence} {
			for _, status := range []string{"active", "unknown"} {
				end := now.Add(-lapsed)
				sub := SubscriptionState{CollectionPolicy: models.CollectionPolicyEngine, Status: status, Rail: "nmi", PeriodEnd: &end}
				d := Decide(sub, EvidenceBundle{Snapshot: snap, Charge: ChargeEvidence{RenewalPaymentAfterPeriodEnd: true}}, now, 0)
				if d.Kind != TransitionNone || d.Reason != "engine_collection_owned" {
					t.Fatalf("cadence %s lapsed %s %s: decision %s (%s), want none/engine_collection_owned", cadence, lapsed, status, d.Kind, d.Reason)
				}
			}
		}
	}
}
