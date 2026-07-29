package reconcile

import (
	"github.com/open-rails/openrails/internal/modules/collection"
	"strings"
	"testing"
	"time"
)

// #632/#633 (via #665): the pure snapshot-resolution law of the ONE decider,
// fixture-tested with hand-built provider snapshots (no DB, no provider calls).
// The subject is an `unknown` row — the #633 resolution path.
func TestDecide_SnapshotLaw(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := now.Add(-10 * 24 * time.Hour) // lapsed 10d ago
	dunningWindow := 30 * 24 * time.Hour
	railSub := "rsub-1"

	sale := func(success bool, at time.Time) RemoteTransaction {
		return RemoteTransaction{TransactionID: "tx-" + at.Format("0102150405"), SubscriptionID: railSub, Type: TransactionTypeSale, Success: success, AmountCents: 5000, Currency: "USD", OccurredAt: at}
	}
	decline := func(at time.Time) RemoteTransaction {
		return RemoteTransaction{TransactionID: "txd-" + at.Format("0102150405"), SubscriptionID: railSub, Type: TransactionTypeDecline, Success: false, OccurredAt: at}
	}
	// #821: a decline whose rail code withdraws the recurring authorization —
	// the ONE decline shape that is certainty.
	hardDecline := func(at time.Time) RemoteTransaction {
		t := decline(at)
		t.DeclineCode = "261" // declined_stop_all_recurring_payments
		return t
	}
	roster := func(status SubscriptionStatus, next *time.Time) RemoteSubscription {
		return RemoteSubscription{RailSubscriptionID: railSub, Status: status, NextBillingAt: next}
	}
	nextEnd := now.Add(20 * 24 * time.Hour)

	cases := []struct {
		name           string
		snap           *RemoteSnapshot
		wantKind       TransitionKind
		wantNewEnd     *time.Time
		wantBackfill   int
		wantRemoteGone bool
		wantCertainty  string
	}{
		{
			name:     "nil snapshot → none (stay unknown)",
			snap:     nil,
			wantKind: TransitionNone,
		},
		{
			name:         "successful renewal charge → renew + backfill, advance to next billing",
			snap:         &RemoteSnapshot{Transactions: []RemoteTransaction{sale(true, periodEnd.Add(time.Hour))}, Subscriptions: []RemoteSubscription{roster(SubscriptionStatusActive, &nextEnd)}},
			wantKind:     TransitionRenew,
			wantNewEnd:   &nextEnd,
			wantBackfill: 1,
		},
		{
			// #367 alignment slack: NMI bills on its own day boundary — a renewal
			// up to a day BEFORE the local period end still counts as the charge.
			name:         "renewal charge just before period end (alignment slack) → renew",
			snap:         &RemoteSnapshot{Transactions: []RemoteTransaction{sale(true, periodEnd.Add(-12*time.Hour))}, Subscriptions: []RemoteSubscription{roster(SubscriptionStatusActive, &nextEnd)}},
			wantKind:     TransitionRenew,
			wantNewEnd:   &nextEnd,
			wantBackfill: 1,
		},
		{
			// #367 doctrine: period adoption alone never grants access — no charge
			// means no renewal-shaped advance, only the provider's clock is adopted.
			name:       "roster active + future next billing, no charge → adopt end (never renew)",
			snap:       &RemoteSnapshot{Subscriptions: []RemoteSubscription{roster(SubscriptionStatusActive, &nextEnd)}},
			wantKind:   TransitionAdoptPeriodEnd,
			wantNewEnd: &nextEnd,
		},
		{
			name:     "roster active but no future boundary, no charge → none (wait for evidence)",
			snap:     &RemoteSnapshot{Subscriptions: []RemoteSubscription{roster(SubscriptionStatusActive, nil)}},
			wantKind: TransitionNone,
		},
		{
			// Ported #367 stalled-remote: roster says the record wedged (NMI next
			// charge in the past / Stripe past_due) → dunning while recoverable.
			name:     "roster past_due, lapse within dunning window → past_due",
			snap:     &RemoteSnapshot{Subscriptions: []RemoteSubscription{roster(SubscriptionStatusPastDue, nil)}},
			wantKind: TransitionPastDue,
		},
		{
			// #821: THE go-live blocker. The only input here is a stale
			// next_billing_date — no decline, no exhausted dunning, and the
			// record is still on the provider's roster. NMI rebills forever, so
			// this is what every dunning customer on an imported book looks
			// like. It must PARK, never cancel and never queue the NMI delete.
			name:           "roster past_due, stale decline-less lapse beyond window → park unknown (no certainty)",
			snap:           &RemoteSnapshot{Subscriptions: []RemoteSubscription{roster(SubscriptionStatusPastDue, nil)}},
			wantKind:       TransitionParkUnknown,
			wantRemoteGone: false,
		},
		{
			name:         "declined renewal within dunning window → past_due + backfill the decline",
			snap:         &RemoteSnapshot{Transactions: []RemoteTransaction{decline(periodEnd.Add(time.Hour))}, Subscriptions: []RemoteSubscription{roster(SubscriptionStatusPastDue, nil)}},
			wantKind:     TransitionPastDue,
			wantBackfill: 1,
		},
		{
			// #821: a SOFT decline beyond the window is a customer mid-dunning
			// on a schedule the rail keeps retrying, not a dead one. Park.
			name:           "soft declined renewal older than dunning window → park unknown (no certainty)",
			snap:           &RemoteSnapshot{Transactions: []RemoteTransaction{decline(periodEnd.Add(time.Hour))}},
			wantKind:       TransitionParkUnknown,
			wantBackfill:   1,
			wantRemoteGone: false,
		},
		{
			// The certainty leg, wired: NMI 261 withdraws the recurring
			// authorization outright. The remote record may still exist, so the
			// apply path queues the deferred delete (#679).
			name:           "NON-RETRYABLE declined renewal older than dunning window → cancel, remote NOT gone (#679)",
			snap:           &RemoteSnapshot{Provider: ProviderNMI, Transactions: []RemoteTransaction{hardDecline(periodEnd.Add(time.Hour))}},
			wantKind:       TransitionCancel,
			wantBackfill:   1,
			wantRemoteGone: false,
			wantCertainty:  collection.CertaintyNonRetryableDecline,
		},
		{
			name:           "stale decline + roster cancelled → cancel, remote gone",
			snap:           &RemoteSnapshot{Transactions: []RemoteTransaction{decline(periodEnd.Add(time.Hour))}, Subscriptions: []RemoteSubscription{roster(SubscriptionStatusCancelled, nil)}},
			wantKind:       TransitionCancel,
			wantBackfill:   1,
			wantRemoteGone: true,
		},
		{
			name:           "stale decline + absent from exhaustive roster → cancel, remote gone",
			snap:           &RemoteSnapshot{Transactions: []RemoteTransaction{decline(periodEnd.Add(time.Hour))}, Subscriptions: []RemoteSubscription{}, Coverage: SnapshotCoverage{SubscriptionsExhaustive: true}},
			wantKind:       TransitionCancel,
			wantBackfill:   1,
			wantRemoteGone: true,
		},
		{
			name:           "roster cancelled → cancel, remote gone",
			snap:           &RemoteSnapshot{Subscriptions: []RemoteSubscription{roster(SubscriptionStatusCancelled, nil)}},
			wantKind:       TransitionCancel,
			wantRemoteGone: true,
		},
		{
			// A roster-declared dead sub is provider certainty; a charge cannot
			// resurrect it (it is still backfilled — money truth vs lifecycle truth).
			name:           "renewal charge + roster cancelled → cancel, remote gone (charge cannot resurrect)",
			snap:           &RemoteSnapshot{Transactions: []RemoteTransaction{sale(true, periodEnd.Add(time.Hour))}, Subscriptions: []RemoteSubscription{roster(SubscriptionStatusCancelled, nil)}},
			wantKind:       TransitionCancel,
			wantBackfill:   1,
			wantRemoteGone: true,
		},
		{
			name:           "roster expired → cancel, remote gone",
			snap:           &RemoteSnapshot{Subscriptions: []RemoteSubscription{roster(SubscriptionStatusExpired, nil)}},
			wantKind:       TransitionCancel,
			wantRemoteGone: true,
		},
		{
			name:           "absent from exhaustive roster → cancel (deleted at provider), remote gone",
			snap:           &RemoteSnapshot{Subscriptions: []RemoteSubscription{}, Coverage: SnapshotCoverage{SubscriptionsExhaustive: true}},
			wantKind:       TransitionCancel,
			wantRemoteGone: true,
		},
		{
			name:     "absent from NON-exhaustive pull → none (stay unknown)",
			snap:     &RemoteSnapshot{Subscriptions: []RemoteSubscription{}},
			wantKind: TransitionNone,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			window := dunningWindow
			if strings.Contains(c.name, "stale decline") || strings.Contains(c.name, "older than dunning window") || strings.Contains(c.name, "beyond window") {
				window = 24 * time.Hour // lapse (10d) now exceeds the window
			}
			pe := periodEnd
			d := Decide(SubscriptionState{Status: "unknown", RailSubscriptionID: railSub, PeriodEnd: &pe},
				EvidenceBundle{Snapshot: c.snap}, now, window)
			if d.Kind != c.wantKind {
				t.Fatalf("kind = %v, want %v", d.Kind, c.wantKind)
			}
			if (d.NewPeriodEnd == nil) != (c.wantNewEnd == nil) {
				t.Fatalf("newPeriodEnd nil-ness mismatch: got %v want %v", d.NewPeriodEnd, c.wantNewEnd)
			}
			if c.wantNewEnd != nil && !d.NewPeriodEnd.Equal(*c.wantNewEnd) {
				t.Fatalf("newPeriodEnd = %v, want %v", d.NewPeriodEnd, c.wantNewEnd)
			}
			if len(d.Backfill) != c.wantBackfill {
				t.Fatalf("backfill = %d, want %d", len(d.Backfill), c.wantBackfill)
			}
			if d.RemoteGone != c.wantRemoteGone {
				t.Fatalf("remoteGone = %v, want %v", d.RemoteGone, c.wantRemoteGone)
			}
			// #821: a TransitionCancel without a named certainty leg is
			// structurally impossible.
			wantCertainty := c.wantCertainty
			if wantCertainty == "" && c.wantKind == TransitionCancel {
				wantCertainty = collection.CertaintyProviderConfirmedDead
			}
			if d.Kind != TransitionCancel {
				wantCertainty = ""
			}
			if d.Certainty != wantCertainty {
				t.Fatalf("certainty = %q, want %q", d.Certainty, wantCertainty)
			}
			if d.Kind == TransitionPastDue {
				want := periodEnd.Add(PeriodGrace)
				if !d.GraceEndsAt.Equal(want) {
					t.Fatalf("graceEndsAt = %v, want period end + PeriodGrace (%v)", d.GraceEndsAt, want)
				}
			}
		})
	}

	// #664: a sub with NO provider handle can never be "absent from an exhaustive
	// roster" — it is unmatchable, so it must stay unknown, never guess-cancelled.
	t.Run("empty rail_subscription_id → none even on exhaustive roster", func(t *testing.T) {
		snap := &RemoteSnapshot{Subscriptions: []RemoteSubscription{}, Coverage: SnapshotCoverage{SubscriptionsExhaustive: true}}
		pe := periodEnd
		d := Decide(SubscriptionState{Status: "unknown", RailSubscriptionID: "", PeriodEnd: &pe},
			EvidenceBundle{Snapshot: snap}, now, dunningWindow)
		if d.Kind != TransitionNone {
			t.Fatalf("kind = %v, want none (no handle = no evidence)", d.Kind)
		}
	})
}

// The #664 first-party law: lapsed active rows enter dunning only with
// ownership evidence on an ours-to-bill rail; everything else parks after the
// grace slack. Stalled past_due rows park; certainty legs cancel.
func TestDecide_FirstPartyLaw(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	lapsed := now.Add(-10 * 24 * time.Hour)
	fresh := now.Add(-time.Hour) // lapsed but within the PeriodGrace slack
	graceGone := now.Add(-2 * time.Hour)

	active := func(rail string, vaulted bool, end time.Time) SubscriptionState {
		return SubscriptionState{Status: "active", Rail: rail, Vaulted: vaulted, RailSubscriptionID: "rs", PeriodEnd: &end}
	}

	cases := []struct {
		name     string
		sub      SubscriptionState
		ev       EvidenceBundle
		wantKind TransitionKind
	}{
		{"vaulted nmi + payment opened period → past_due",
			active("nmi", true, lapsed),
			EvidenceBundle{Charge: ChargeEvidence{PaymentOpenedCurrentPeriod: true}},
			TransitionPastDue},
		{"vaulted nmi + watermark newer than period end → past_due",
			active("nmi", true, lapsed),
			EvidenceBundle{WatermarkNewerThanPeriodEnd: true},
			TransitionPastDue},
		{"vaulted nmi, NO evidence (the #664 imported shape) → park",
			active("nmi", true, lapsed),
			EvidenceBundle{},
			TransitionParkUnknown},
		{"vault-less nmi with payment evidence → park (provider-auto-billed, never ours)",
			active("nmi", false, lapsed),
			EvidenceBundle{Charge: ChargeEvidence{PaymentOpenedCurrentPeriod: true}},
			TransitionParkUnknown},
		{"ccbill lapsed → park",
			active("ccbill", false, lapsed),
			EvidenceBundle{},
			TransitionParkUnknown},
		{"stripe lapsed → park",
			active("stripe", false, lapsed),
			EvidenceBundle{},
			TransitionParkUnknown},
		{"renewal payment recorded after period end → none (advance path owns it)",
			active("ccbill", false, lapsed),
			EvidenceBundle{Charge: ChargeEvidence{RenewalPaymentAfterPeriodEnd: true}},
			TransitionNone},
		{"lapsed but within the grace slack, no evidence → none (wait)",
			active("ccbill", false, fresh),
			EvidenceBundle{},
			TransitionNone},
		{"current period → none",
			active("nmi", true, now.Add(20*24*time.Hour)),
			EvidenceBundle{},
			TransitionNone},
		{"past_due, grace elapsed, no retry scheduled → park (dunning stalled)",
			SubscriptionState{Status: "past_due", Rail: "nmi", Vaulted: true, PeriodEnd: &lapsed, GraceEndsAt: &graceGone},
			EvidenceBundle{},
			TransitionParkUnknown},
		{"past_due, grace elapsed, retry scheduled → none (dunning working)",
			SubscriptionState{Status: "past_due", Rail: "nmi", Vaulted: true, PeriodEnd: &lapsed, GraceEndsAt: &graceGone, NextRetryScheduled: true},
			EvidenceBundle{},
			TransitionNone},
		{"past_due, grace elapsed, non-retryable decline → cancel (certainty leg)",
			SubscriptionState{Status: "past_due", Rail: "nmi", Vaulted: true, PeriodEnd: &lapsed, GraceEndsAt: &graceGone},
			EvidenceBundle{Charge: ChargeEvidence{NonRetryableDecline: true}},
			TransitionCancel},
		{"past_due, grace elapsed, dunning exhausted → cancel (certainty leg)",
			SubscriptionState{Status: "past_due", Rail: "nmi", Vaulted: true, PeriodEnd: &lapsed, GraceEndsAt: &graceGone},
			EvidenceBundle{Charge: ChargeEvidence{RetryAttempts: 4, DunningMaxAttempts: 4}},
			TransitionCancel},
		{"pending → none (signup/pending_stale own it)",
			SubscriptionState{Status: "pending", Rail: "nmi", Vaulted: true, PeriodEnd: &lapsed},
			EvidenceBundle{},
			TransitionNone},
		{"cancelled → none (terminal)",
			SubscriptionState{Status: "cancelled", Rail: "nmi", Vaulted: true, PeriodEnd: &lapsed},
			EvidenceBundle{},
			TransitionNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Decide(c.sub, c.ev, now, 0)
			if d.Kind != c.wantKind {
				t.Fatalf("kind = %v, want %v (reason %q)", d.Kind, c.wantKind, d.Reason)
			}
			if d.Kind == TransitionPastDue {
				want := c.sub.PeriodEnd.Add(PeriodGrace)
				if !d.GraceEndsAt.Equal(want) {
					t.Fatalf("graceEndsAt = %v, want %v", d.GraceEndsAt, want)
				}
			}
		})
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
