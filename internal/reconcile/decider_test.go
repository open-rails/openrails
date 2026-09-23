package reconcile

import (
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/internal/db/models"
)

func TestLifecycleDecisionApplierClockOwnership(t *testing.T) {
	now := time.Date(2041, time.March, 4, 5, 6, 7, 0, time.UTC)
	applier := NewDecisionApplier(nil, nil, clockwork.NewFakeClockAt(now))

	if got := applier.clock.Now(); !got.Equal(now) {
		t.Fatalf("decision clock = %v, want %v", got, now)
	}
	if got := applier.LC.Clock().Now(); !got.Equal(now) {
		t.Fatalf("lifecycle clock = %v, want %v", got, now)
	}

	later := now.Add(48 * time.Hour)
	applier.SetClock(clockwork.NewFakeClockAt(later))
	if got := applier.clock.Now(); !got.Equal(later) {
		t.Fatalf("updated decision clock = %v, want %v", got, later)
	}
	if got := applier.LC.Clock().Now(); !got.Equal(later) {
		t.Fatalf("updated lifecycle clock = %v, want %v", got, later)
	}
}

// Backfill only includes charges at/after the lapsed period end (older history is
// out of this subscription's reconcile scope).
func TestSubscriptionBackfill_WindowsByPeriodEnd(t *testing.T) {
	periodEnd := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	txns := []RemoteTransaction{
		{TransactionID: "old", OccurredAt: periodEnd.Add(-time.Hour)},
		{TransactionID: "boundary", OccurredAt: periodEnd},
		{TransactionID: "new", OccurredAt: periodEnd.Add(time.Hour)},
	}
	got := subscriptionBackfill(txns, periodEnd)
	if len(got) != 2 {
		t.Fatalf("backfill = %d, want 2 (boundary + new)", len(got))
	}
	for _, txn := range got {
		if txn.TransactionID == "old" {
			t.Fatal("old charge (before period end) must be excluded")
		}
	}
}

func TestFirstPartyDecisionRespectsEngineOwnership(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	end := now.Add(-100 * 24 * time.Hour)
	grace := now.Add(-time.Hour)
	for _, status := range []string{"active", "past_due", "unknown"} {
		for _, owned := range []bool{false, true} {
			sub := SubscriptionState{CollectionPolicy: models.CollectionPolicyEngine, Status: status, Rail: "nmi", HasPaymentMethod: true, PeriodEnd: &end, GraceEndsAt: &grace}
			evidence := EvidenceBundle{Charge: ChargeEvidence{PaymentOpenedCurrentPeriod: owned}, WatermarkNewerThanPeriodEnd: owned}
			if got := Decide(sub, evidence, now, 0); got.Kind != TransitionNone {
				t.Fatalf("engine %s ownership=%v: %+v", status, owned, got)
			}
			sub.CollectionPolicy = models.CollectionPolicyProviderDunning
			if got := Decide(sub, evidence, now, 0); status != "unknown" && got.Kind == TransitionNone {
				t.Fatalf("native %s ownership=%v lost its existing decision", status, owned)
			}
		}
	}
	terminal := SubscriptionState{CollectionPolicy: models.CollectionPolicyEngine, Status: "past_due", Rail: "nmi", PeriodEnd: &end, GraceEndsAt: &grace}
	proof := EvidenceBundle{Charge: ChargeEvidence{NonRetryableDecline: true, LastAttemptAt: now}}
	if got := Decide(terminal, proof, now, 0); got.Kind != TransitionCancel {
		t.Fatalf("engine terminal charge evidence lost safety decision: %+v", got)
	}
}
