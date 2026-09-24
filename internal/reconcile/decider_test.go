package reconcile

import (
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/internal/db/models"
)

func TestFutureProviderScheduleDoesNotEraseDeclinedRenewal(t *testing.T) {
	end := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	next := end.Add(30 * 24 * time.Hour)
	snapshot := &RemoteSnapshot{
		Provider:      ProviderNMI,
		Subscriptions: []RemoteSubscription{{RailSubscriptionID: "schedule", Status: SubscriptionStatusActive, NextBillingAt: &next}},
		Transactions:  []RemoteTransaction{{SubscriptionID: "schedule", Type: TransactionTypeDecline, OccurredAt: end, DeclineCode: "202"}},
	}
	got := decideFromSnapshot("schedule", nil, nil, end, snapshot, end.Add(time.Hour), 30*24*time.Hour)
	if got.Kind != TransitionPastDue || got.NewPeriodEnd != nil {
		t.Fatalf("next scheduled date is not payment evidence: %+v", got)
	}
	snapshot.Transactions = nil
	if got := decideFromSnapshot("schedule", nil, nil, end, snapshot, end.Add(time.Hour), 30*24*time.Hour); got.Kind != TransitionAdoptPeriodEnd {
		t.Fatalf("schedule adoption without a declined renewal changed: %+v", got)
	}
}

// A daily period's previous charge (a day before its end) is not its renewal,
// and a roster that advanced past the local period without an attributable
// charge is inconclusive rather than adopted.
func TestShortPeriodAlignmentAndAdvancedRoster(t *testing.T) {
	end := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	start := end.Add(-24 * time.Hour)
	next := end.Add(24 * time.Hour)
	snapshot := &RemoteSnapshot{
		Provider:      ProviderNMI,
		Subscriptions: []RemoteSubscription{{RailSubscriptionID: "daily", Status: SubscriptionStatusActive, NextBillingAt: &next}},
		Transactions: []RemoteTransaction{
			{SubscriptionID: "daily", TransactionID: "previous", Type: TransactionTypeSale, Success: true, OccurredAt: start.Add(2 * time.Hour)},
			{SubscriptionID: "daily", TransactionID: "declined", Type: TransactionTypeDecline, OccurredAt: end.Add(2 * time.Hour), DeclineCode: "202"},
		},
	}
	if got := decideFromSnapshot("daily", &start, &end, end, snapshot, end.Add(3*time.Hour), 7*24*time.Hour); got.Kind != TransitionPastDue {
		t.Fatalf("a daily decline is masked by the previous day's charge: %+v", got)
	}
	snapshot.Transactions = nil
	if got := decideFromSnapshot("daily", &start, &end, end, snapshot, end.Add(3*time.Hour), 7*24*time.Hour); got.Kind != TransitionNone {
		t.Fatalf("an advanced roster without its charge is adopted: %+v", got)
	}
	if got := AlignmentSlack(&start, &end); got != 12*time.Hour {
		t.Fatalf("daily slack = %v", got)
	}
}

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
