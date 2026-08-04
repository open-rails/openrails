package reconcile

import (
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/modules/collection"
)

// #835 — the evidence-staleness floor. The arming gate protects the FIRST pass;
// it does nothing about the second pass acting on a record that is itself older
// than anything this deployment has ever observed. On an imported legacy book
// every decline on file arrived WITH the data.

// staleDeclineBundle is the realistic legacy shape: the roster lists the row
// (alive at the rail, wedged), and the snapshot carries one hard decline whose
// code IS non-retryable — real certainty by kind, but inherited by provenance.
func staleDeclineBundle(now time.Time, declinedAt time.Time, floor time.Time) (SubscriptionState, EvidenceBundle) {
	periodEnd := now.AddDate(0, 0, -60) // beyond DefaultDunningWindow
	railSub := "rsub-legacy"
	snap := &RemoteSnapshot{
		Provider:      ProviderNMI,
		FetchedAt:     now,
		Subscriptions: []RemoteSubscription{{RailSubscriptionID: railSub, Status: SubscriptionStatusPastDue, NextBillingAt: &periodEnd}},
		Transactions: []RemoteTransaction{{
			TransactionID: "txn-legacy", SubscriptionID: railSub,
			Type: TransactionTypeDecline, OccurredAt: declinedAt,
			DeclineCode: "261", // stop all recurring payments — genuinely terminal
		}},
	}
	state := SubscriptionState{
		Status: "active", Rail: "nmi", HasPaymentMethod: true,
		RailSubscriptionID: railSub, PeriodEnd: &periodEnd,
	}
	return state, EvidenceBundle{Snapshot: snap, EvidenceFloor: floor}
}

func TestDecide_EvidencePredatingTheFirstPullNeverCancels(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	floor := now.AddDate(0, 0, -7) // this deployment's first completed pull

	state, ev := staleDeclineBundle(now, now.AddDate(0, 0, -50), floor)
	d := Decide(state, ev, now, 0)

	if d.Kind != TransitionParkUnknown {
		t.Fatalf("kind = %v (certainty=%q reason=%q), want park_unknown: the decline predates the first pull",
			d.Kind, d.Certainty, d.Reason)
	}
	if !d.EvidenceFloored {
		t.Fatal("a floored cancel must be flagged so the planes can raise a finding, not silently no-op")
	}
	if d.Reason != "declined_renewal_beyond_window_evidence_predates_first_pull" {
		t.Fatalf("reason = %q", d.Reason)
	}
}

// The converse: the floor must not freeze the engine. The same row, declined
// AFTER the deployment started observing this merchant, still converges.
func TestDecide_EvidenceAfterTheFirstPullStillCancels(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	floor := now.AddDate(0, 0, -7)

	state, ev := staleDeclineBundle(now, now.AddDate(0, 0, -3), floor)
	d := Decide(state, ev, now, 0)

	if d.Kind != TransitionCancel {
		t.Fatalf("kind = %v reason=%q, want cancel: this decline is ours, we watched it happen", d.Kind, d.Reason)
	}
	if d.Certainty != collection.CertaintyNonRetryableDecline {
		t.Fatalf("certainty = %q", d.Certainty)
	}
	if d.EvidenceFloored {
		t.Fatal("evidence from after the first pull is not floored")
	}
}

// The provider's CURRENT word is always fresh: a roster read is an observation
// this pass just made, whatever the row's history says.
func TestDecide_RosterDeadIsDatedByThisPassNotByHistory(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	floor := now.AddDate(0, 0, -7)
	periodEnd := now.AddDate(0, 0, -400)
	railSub := "rsub-dead"

	snap := &RemoteSnapshot{
		Provider:      ProviderNMI,
		FetchedAt:     now,
		Subscriptions: []RemoteSubscription{{RailSubscriptionID: railSub, Status: SubscriptionStatusCancelled}},
	}
	d := Decide(SubscriptionState{
		Status: "active", Rail: "nmi", RailSubscriptionID: railSub, PeriodEnd: &periodEnd,
	}, EvidenceBundle{Snapshot: snap, EvidenceFloor: floor}, now, 0)

	if d.Kind != TransitionCancel || d.Certainty != collection.CertaintyProviderConfirmedDead {
		t.Fatalf("roster-dead = %v/%q; the floor must not block the provider's own current word", d.Kind, d.Certainty)
	}
}

// A certainty leg with NO timestamp is refused rather than assumed fresh. This
// is the shape of first-party dunning certainty today: ChargeEvidence carries
// the legs but nothing populates LastAttemptAt.
func TestDecide_UndatedFirstPartyCertaintyIsRefused(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	floor := now.AddDate(0, 0, -7)
	periodEnd := now.AddDate(0, 0, -30)
	grace := periodEnd.Add(PeriodGrace)
	state := SubscriptionState{Status: "past_due", Rail: "nmi", HasPaymentMethod: true, PeriodEnd: &periodEnd, GraceEndsAt: &grace}

	undated := Decide(state, EvidenceBundle{
		EvidenceFloor: floor,
		Charge:        ChargeEvidence{RetryAttempts: 5, DunningMaxAttempts: 5},
	}, now, 0)
	if undated.Kind != TransitionParkUnknown || !undated.EvidenceFloored {
		t.Fatalf("undated dunning certainty = %v (floored=%v); undated evidence is not fresh evidence", undated.Kind, undated.EvidenceFloored)
	}
	if undated.Reason != "dunning_exhausted_certainty_evidence_undated" {
		t.Fatalf("reason = %q", undated.Reason)
	}

	dated := Decide(state, EvidenceBundle{
		EvidenceFloor: floor,
		Charge:        ChargeEvidence{RetryAttempts: 5, DunningMaxAttempts: 5, LastAttemptAt: now.AddDate(0, 0, -1)},
	}, now, 0)
	if dated.Kind != TransitionCancel || dated.Certainty != collection.CertaintyDunningExhausted {
		t.Fatalf("dated dunning certainty = %v/%q, want cancel", dated.Kind, dated.Certainty)
	}
}

// A caller that supplies no floor gets the STRICTER answer, never a permissive
// one: with no first pull on record, only what THIS pass observed counts.
func TestDecide_MissingFloorFallsBackToThisPassObservation(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	state, ev := staleDeclineBundle(now, now.AddDate(0, 0, -50), time.Time{})
	if d := Decide(state, ev, now, 0); d.Kind != TransitionParkUnknown || !d.EvidenceFloored {
		t.Fatalf("no floor + dated snapshot = %v (floored=%v); the fallback must be the snapshot's own FetchedAt", d.Kind, d.EvidenceFloored)
	}
}
