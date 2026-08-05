package reconcile

import (
	"github.com/open-rails/openrails/internal/modules/collection"
	"testing"
	"time"
)

// #821: a roster row wedged past its next-billing date is the NORMAL state of
// every dunning customer on a real NMI book — the rail retries forever and
// stops advancing the date while rebills fail. Staleness is a clock reading,
// not evidence, so it must NEVER produce a terminal cancel (which queues the
// irreversible NMI vault delete), no matter how old it gets.
func TestDecide_StaleRosterDateNeverCancels(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	railSub := "rsub-stale"

	for _, staleDays := range []int{15, 30, 90, 365, 3 * 365} {
		periodEnd := now.AddDate(0, 0, -staleDays)
		nextBilling := periodEnd // NMI wedged the schedule here and never moved it
		snap := &RemoteSnapshot{
			Provider: ProviderNMI,
			// The roster LISTS the subscription — it is alive at the rail.
			Subscriptions: []RemoteSubscription{{
				RailSubscriptionID: railSub,
				Status:             SubscriptionStatusPastDue, // inferred from the date, not declared
				NextBillingAt:      &nextBilling,
			}},
			Coverage: SnapshotCoverage{SubscriptionsExhaustive: true},
		}

		for _, status := range []string{"active", "past_due", "unknown"} {
			d := Decide(SubscriptionState{
				Status: status, Rail: "nmi", HasPaymentMethod: true,
				RailSubscriptionID: railSub, PeriodEnd: &periodEnd,
			}, EvidenceBundle{Snapshot: snap}, now, 0)

			if d.Kind == TransitionCancel {
				t.Fatalf("stale %dd / local %s: cancelled on a date comparison alone (certainty=%q reason=%q)",
					staleDays, status, d.Certainty, d.Reason)
			}
			if d.Kind != TransitionParkUnknown {
				t.Fatalf("stale %dd / local %s: kind = %v, want park_unknown", staleDays, status, d.Kind)
			}
		}
	}
}

// The same row, once REAL certainty arrives, does cancel — the fix removes the
// fabricated path, not the ability to converge a genuinely dead subscription.
func TestDecide_StaleRosterDateCancelsOnCertainty(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := now.AddDate(0, 0, -90)
	railSub := "rsub-stale"
	snap := &RemoteSnapshot{
		Provider:      ProviderNMI,
		Subscriptions: []RemoteSubscription{{RailSubscriptionID: railSub, Status: SubscriptionStatusPastDue, NextBillingAt: &periodEnd}},
	}
	state := SubscriptionState{Status: "past_due", Rail: "nmi", HasPaymentMethod: true, RailSubscriptionID: railSub, PeriodEnd: &periodEnd}

	cases := map[string]struct {
		charge    ChargeEvidence
		certainty string
	}{
		"non-retryable decline": {ChargeEvidence{NonRetryableDecline: true}, collection.CertaintyNonRetryableDecline},
		"dunning exhausted":     {ChargeEvidence{RetryAttempts: 5, DunningMaxAttempts: 5}, collection.CertaintyDunningExhausted},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			d := Decide(state, EvidenceBundle{Snapshot: snap, Charge: c.charge}, now, 0)
			if d.Kind != TransitionCancel {
				t.Fatalf("kind = %v, want cancel", d.Kind)
			}
			if d.Certainty != c.certainty {
				t.Fatalf("certainty = %q, want %q", d.Certainty, c.certainty)
			}
		})
	}
}

// The chokepoint itself: every TransitionCancel the decider can emit names the
// leg that justified it. An unnamed cancel cannot exist.
func TestGateCancelCertainty_NoCancelWithoutALeg(t *testing.T) {
	base := Decision{Kind: TransitionCancel, Reason: "made_up"}

	if got := gateCancelCertainty(base, EvidenceBundle{}); got.Kind != TransitionParkUnknown {
		t.Fatalf("uncertain cancel survived as %v", got.Kind)
	} else if got.Reason != "made_up_no_certainty" {
		t.Fatalf("reason = %q", got.Reason)
	}

	gone := base
	gone.RemoteGone = true
	if got := gateCancelCertainty(gone, EvidenceBundle{}); got.Kind != TransitionCancel || got.Certainty != collection.CertaintyProviderConfirmedDead {
		t.Fatalf("provider-confirmed cancel = %v/%q", got.Kind, got.Certainty)
	}

	withLeg := gateCancelCertainty(base, EvidenceBundle{Charge: ChargeEvidence{NonRetryableDecline: true}})
	if withLeg.Kind != TransitionCancel || withLeg.Certainty != collection.CertaintyNonRetryableDecline {
		t.Fatalf("first-party certainty cancel = %v/%q", withLeg.Kind, withLeg.Certainty)
	}
}

func TestDeciderCancelCertaintyUsesTheOr870Classifier(t *testing.T) {
	// The decider's terminal leg fires for bucket 3 and NOTHING else: a bucket-2
	// card problem beyond the dunning window is a customer who can still fix it,
	// and an unreadable code is missing evidence.
	cases := []struct {
		rail, code string
		want       bool
	}{
		{"nmi", "261", true},  // stop all recurring payments
		{"nmi", "262", true},  // stop this recurring program
		{"nmi", "252", true},  // stolen card
		{"nmi", "202", false}, // insufficient funds — dun, never cancel
		{"nmi", "201", false}, // do not honor — bucket 2, they can fix it
		{"nmi", "223", false}, // expired card — bucket 2
		{"nmi", "264", false}, // "retry in a few days" is literally retryable
		{"nmi", "420", false}, // communication error — ours
		{"nmi", "declined_stop_all_recurring_payments", true},
		{"nmi", "nmi_declined_stop_all_recurring_payments", true},
		{"nmi", "", false},
		{"nmi", "wat", false}, // unrecognized => retryable
		{"stripe", "revocation_of_all_authorizations", true},
		{"stripe", "insufficient_funds", false},
		{"ccbill", "261", false}, // no trusted vocabulary
		{"solana", "anything", false},
	}
	for _, c := range cases {
		got := collection.ClassifyDecline(c.rail, c.code) == collection.DeclineNonRecoverable
		if got != c.want {
			t.Errorf("ClassifyDecline(%q, %q)==NonRecoverable = %v, want %v", c.rail, c.code, got, c.want)
		}
	}
}
