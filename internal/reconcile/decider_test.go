package reconcile

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/collection"
)

const oneDay = 24 * time.Hour

var decideNow = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

func rel(d time.Duration) *time.Time { t := decideNow.Add(d); return &t }

func rosterSub(status SubscriptionStatus, next *time.Time) *RemoteSubscription {
	return &RemoteSubscription{RailSubscriptionID: "rs", CustomerID: "cus_rs", Status: status, NextBillingAt: next}
}

func rtx(sub string, typ TransactionType, ok bool, at time.Time, code string) RemoteTransaction {
	return RemoteTransaction{TransactionID: fmt.Sprintf("%s-%s-%d", sub, typ, at.Unix()), SubscriptionID: sub, Type: typ, Success: ok, OccurredAt: at, DeclineCode: code}
}

// Provider truth decides first; a probe-resolved `unknown` row isolates it.
func TestDecideSnapshotLaw(t *testing.T) {
	e5, e30, e60 := rel(-5*oneDay), rel(-30*oneDay), rel(-60*oneDay)
	dailyEnd := rel(-3 * time.Hour)
	dailyStart := rel(-27 * time.Hour)
	next := rel(25 * oneDay)
	cases := []struct {
		name       string
		provider   Provider
		start, end *time.Time
		roster     *RemoteSubscription
		exhaustive bool
		txns       []RemoteTransaction
		charge     ChargeEvidence
		floor      *time.Time
		noFloor    bool

		want      TransitionKind
		reason    string
		certainty string
		gone      bool
		floored   bool
		backfill  int
	}{
		{name: "verified renewal charge renews", end: e5, roster: rosterSub(SubscriptionStatusActive, next),
			txns: []RemoteTransaction{rtx("rs", TransactionTypeSale, true, e5.Add(time.Hour), "")},
			want: TransitionRenew, reason: "verified_renewal_charge", backfill: 1},
		{name: "renewal a day early is still this period's renewal", end: e5, roster: rosterSub(SubscriptionStatusActive, next),
			txns: []RemoteTransaction{rtx("rs", TransactionTypeSale, true, e5.Add(-20*time.Hour), "")},
			want: TransitionRenew, backfill: 1},
		{name: "daily period: previous day's charge does not mask the decline", start: dailyStart, end: dailyEnd,
			roster: rosterSub(SubscriptionStatusActive, rel(21*time.Hour)),
			txns: []RemoteTransaction{
				rtx("rs", TransactionTypeSale, true, dailyStart.Add(2*time.Hour), ""),
				rtx("rs", TransactionTypeDecline, false, dailyEnd.Add(2*time.Hour), "202"),
			},
			want: TransitionPastDue, reason: "declined_renewal_within_window", backfill: 1},
		{name: "daily period: NMI roster advanced without its charge is inconclusive", start: dailyStart, end: dailyEnd,
			roster: rosterSub(SubscriptionStatusActive, rel(21*time.Hour)), want: TransitionNone},
		{name: "stripe adopts a future boundary without a charge", provider: ProviderStripe, end: e5,
			roster: rosterSub(SubscriptionStatusActive, next), want: TransitionAdoptPeriodEnd, reason: "roster_alive_future_boundary"},
		{name: "NMI boundary within half a period adopts", start: rel(-31 * oneDay), end: rel(-oneDay),
			roster: rosterSub(SubscriptionStatusActive, rel(3*oneDay)), want: TransitionAdoptPeriodEnd},
		{name: "NMI boundary past a whole period needs a probe", end: e5, roster: rosterSub(SubscriptionStatusActive, next), want: TransitionNone},
		{name: "future schedule cannot erase a declined renewal", end: e5, roster: rosterSub(SubscriptionStatusActive, next),
			txns: []RemoteTransaction{rtx("rs", TransactionTypeDecline, false, e5.Add(time.Hour), "202")},
			want: TransitionPastDue, reason: "declined_renewal_within_window", backfill: 1},
		{name: "a failed sale is a decline", end: e5,
			txns: []RemoteTransaction{rtx("rs", TransactionTypeSale, false, e5.Add(time.Hour), "")},
			want: TransitionPastDue, backfill: 1},
		{name: "soft decline beyond the window parks", end: e30, roster: rosterSub(SubscriptionStatusPastDue, nil),
			txns: []RemoteTransaction{rtx("rs", TransactionTypeDecline, false, e30.Add(time.Hour), "202")},
			want: TransitionParkUnknown, reason: "declined_renewal_beyond_window_no_certainty", backfill: 1},
		{name: "hard decline beyond the window cancels with the remote alive", end: e30, roster: rosterSub(SubscriptionStatusPastDue, nil),
			txns: []RemoteTransaction{rtx("rs", TransactionTypeDecline, false, e30.Add(time.Hour), "261")},
			want: TransitionCancel, reason: "declined_renewal_beyond_window", certainty: collection.CertaintyNonRetryableDecline, backfill: 1},
		{name: "legacy hard decline predating the first pull parks", end: e60, floor: rel(-7 * oneDay), roster: rosterSub(SubscriptionStatusPastDue, e60),
			txns: []RemoteTransaction{rtx("rs", TransactionTypeDecline, false, decideNow.Add(-50*oneDay), "261")},
			want: TransitionParkUnknown, reason: "declined_renewal_beyond_window_evidence_predates_first_pull", floored: true, backfill: 1},
		{name: "hard decline after the first pull cancels", end: e60, floor: rel(-7 * oneDay), roster: rosterSub(SubscriptionStatusPastDue, e60),
			txns: []RemoteTransaction{rtx("rs", TransactionTypeDecline, false, decideNow.Add(-3*oneDay), "261")},
			want: TransitionCancel, certainty: collection.CertaintyNonRetryableDecline, backfill: 1},
		{name: "a missing floor falls back to this pass's observation", end: e60, noFloor: true, roster: rosterSub(SubscriptionStatusPastDue, e60),
			txns: []RemoteTransaction{rtx("rs", TransactionTypeDecline, false, decideNow.Add(-50*oneDay), "261")},
			want: TransitionParkUnknown, floored: true, backfill: 1},
		{name: "roster past_due within the window dunning", end: e5, roster: rosterSub(SubscriptionStatusPastDue, e5),
			want: TransitionPastDue, reason: "roster_past_due_within_window"},
		{name: "roster past_due beyond the window parks", end: e30, roster: rosterSub(SubscriptionStatusPastDue, e30),
			want: TransitionParkUnknown, reason: "roster_past_due_beyond_window_no_certainty"},
		{name: "roster past_due beyond the window cancels on dated exhaustion", end: e30, roster: rosterSub(SubscriptionStatusPastDue, e30),
			charge: ChargeEvidence{RetryAttempts: 3, DunningMaxAttempts: 3, LastAttemptAt: decideNow.Add(-oneDay)},
			want:   TransitionCancel, certainty: collection.CertaintyDunningExhausted},
		{name: "roster dead is dated by this pass, not by history", end: rel(-400 * oneDay), floor: rel(-7 * oneDay), roster: rosterSub(SubscriptionStatusCancelled, nil),
			want: TransitionCancel, reason: "roster_dead", certainty: collection.CertaintyProviderConfirmedDead, gone: true},
		{name: "a renewal charge cannot resurrect an expired roster", end: e5, roster: rosterSub(SubscriptionStatusExpired, nil),
			txns: []RemoteTransaction{rtx("rs", TransactionTypeSale, true, e5.Add(time.Hour), "")},
			want: TransitionCancel, certainty: collection.CertaintyProviderConfirmedDead, gone: true, backfill: 1},
		{name: "absent from an exhaustive roster is gone", end: e5, exhaustive: true,
			want: TransitionCancel, reason: "absent_from_exhaustive_roster", certainty: collection.CertaintyProviderConfirmedDead, gone: true},
		{name: "absent from a partial roster is inconclusive", end: e5, want: TransitionNone},
		{name: "another subscription's events are ignored", provider: ProviderStripe, end: e5, roster: rosterSub(SubscriptionStatusActive, next),
			txns: []RemoteTransaction{rtx("other", TransactionTypeDecline, false, e5.Add(time.Hour), "261")},
			want: TransitionAdoptPeriodEnd},
		{name: "backfill starts at the aligned cutoff and keeps every event type", provider: ProviderStripe, end: e5, roster: rosterSub(SubscriptionStatusActive, next),
			txns: []RemoteTransaction{
				rtx("rs", TransactionTypeRefund, true, e5.Add(-oneDay-time.Second), ""),
				rtx("rs", TransactionTypeRefund, true, e5.Add(-oneDay), ""),
				rtx("rs", TransactionTypeChargeback, true, e5.Add(time.Hour), ""),
			},
			want: TransitionAdoptPeriodEnd, backfill: 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			provider := c.provider
			if provider == "" {
				provider = ProviderNMI
			}
			snap := &RemoteSnapshot{Provider: provider, FetchedAt: decideNow, Transactions: c.txns,
				Coverage: SnapshotCoverage{SubscriptionsExhaustive: c.exhaustive}}
			if c.roster != nil {
				snap.Subscriptions = []RemoteSubscription{*c.roster}
			}
			ev := EvidenceBundle{Snapshot: snap, Charge: c.charge, EvidenceFloor: decideNow.Add(-90 * oneDay)}
			if c.floor != nil {
				ev.EvidenceFloor = *c.floor
			}
			if c.noFloor {
				ev.EvidenceFloor = time.Time{}
			}
			sub := SubscriptionState{Status: "unknown", Rail: "nmi", HasPaymentMethod: true, RailSubscriptionID: "rs", PeriodStart: c.start, PeriodEnd: c.end}

			d := Decide(sub, ev, decideNow, 0)
			require.Equal(t, c.want, d.Kind, "reason=%q certainty=%q", d.Reason, d.Certainty)
			if c.reason != "" {
				require.Equal(t, c.reason, d.Reason)
			}
			require.Equal(t, c.certainty, d.Certainty)
			require.Equal(t, c.gone, d.RemoteGone)
			require.Equal(t, c.floored, d.EvidenceFloored)
			require.Len(t, d.Backfill, c.backfill)
			if c.roster != nil {
				require.Equal(t, "cus_rs", d.RemoteCustomerID, "customer id survives every outcome")
			}
			switch d.Kind {
			case TransitionRenew, TransitionAdoptPeriodEnd:
				require.Equal(t, c.roster.NextBillingAt, d.NewPeriodEnd)
			case TransitionPastDue:
				require.Equal(t, c.end.Add(PeriodGrace), d.GraceEndsAt)
			case TransitionCancel:
				require.False(t, d.EvidenceAt.Before(ev.EvidenceFloor), "cancel evidence %v predates floor", d.EvidenceAt)
			}
		})
	}
}

// Without a provider snapshot only first-party ownership and certainty legs move a row.
func TestDecideFirstPartyLaw(t *testing.T) {
	floor := decideNow.Add(-7 * oneDay)
	exhausted := func(at time.Time) ChargeEvidence {
		return ChargeEvidence{RetryAttempts: 5, DunningMaxAttempts: 5, LastAttemptAt: at}
	}
	cases := []struct {
		name       string
		status     string
		rail       string
		noPM       bool
		policy     models.CollectionPolicy
		end, grace *time.Time
		retry      bool
		charge     ChargeEvidence
		watermark  bool

		want      TransitionKind
		reason    string
		certainty string
		floored   bool
	}{
		{name: "active inside its period", status: "active", end: rel(oneDay), want: TransitionNone},
		{name: "recorded renewal payment belongs to the advance path", status: "active", end: rel(-5 * oneDay),
			charge: ChargeEvidence{RenewalPaymentAfterPeriodEnd: true, PaymentOpenedCurrentPeriod: true}, want: TransitionNone, reason: "renewal_payment_recorded"},
		{name: "ours to bill with an opened payment dunning", status: "active", end: rel(-5 * oneDay),
			charge: ChargeEvidence{PaymentOpenedCurrentPeriod: true}, want: TransitionPastDue, reason: "period_overdue_ownership_evidence"},
		{name: "ours to bill with a fresh watermark dunning", status: "active", end: rel(-5 * oneDay), watermark: true, want: TransitionPastDue},
		{name: "a provider-billed rail is never ours", status: "active", rail: "ccbill", end: rel(-5 * oneDay),
			charge: ChargeEvidence{PaymentOpenedCurrentPeriod: true}, want: TransitionParkUnknown, reason: "no_ownership_evidence"},
		{name: "a vault-less NMI row is never ours", status: "active", noPM: true, end: rel(-5 * oneDay), watermark: true, want: TransitionParkUnknown},
		{name: "a provider-owned schedule is never ours", status: "active", policy: models.CollectionPolicyProvider, end: rel(-5 * oneDay), watermark: true, want: TransitionParkUnknown},
		{name: "lapse inside the grace debounce waits", status: "active", end: rel(-oneDay), want: TransitionNone, reason: "within_grace_slack"},
		{name: "stalled dunning parks", status: "past_due", end: rel(-20 * oneDay), grace: rel(-time.Hour), want: TransitionParkUnknown, reason: "dunning_stalled_past_grace"},
		{name: "a scheduled retry keeps dunning", status: "past_due", end: rel(-20 * oneDay), grace: rel(-time.Hour), retry: true, want: TransitionNone},
		{name: "grace still running", status: "past_due", end: rel(-oneDay), grace: rel(oneDay), want: TransitionNone},
		{name: "dated non-retryable decline cancels", status: "past_due", end: rel(-20 * oneDay), grace: rel(-time.Hour),
			charge: ChargeEvidence{NonRetryableDecline: true, LastAttemptAt: decideNow.Add(-oneDay)},
			want:   TransitionCancel, reason: "dunning_exhausted_certainty", certainty: collection.CertaintyNonRetryableDecline},
		{name: "undated exhaustion is refused", status: "past_due", end: rel(-30 * oneDay), grace: rel(-time.Hour), charge: exhausted(time.Time{}),
			want: TransitionParkUnknown, reason: "dunning_exhausted_certainty_evidence_undated", floored: true},
		{name: "exhaustion predating the first pull is refused", status: "past_due", end: rel(-30 * oneDay), grace: rel(-time.Hour), charge: exhausted(decideNow.Add(-20 * oneDay)),
			want: TransitionParkUnknown, reason: "dunning_exhausted_certainty_evidence_predates_first_pull", floored: true},
		{name: "dated exhaustion cancels", status: "past_due", end: rel(-30 * oneDay), grace: rel(-time.Hour), charge: exhausted(decideNow.Add(-oneDay)),
			want: TransitionCancel, certainty: collection.CertaintyDunningExhausted},
		{name: "attempts without a policy max are not exhaustion", status: "past_due", end: rel(-30 * oneDay), grace: rel(-time.Hour),
			charge: ChargeEvidence{RetryAttempts: 5, LastAttemptAt: decideNow}, want: TransitionParkUnknown, reason: "dunning_stalled_past_grace"},
		{name: "unknown waits for the provider", status: "unknown", end: rel(-30 * oneDay), watermark: true, want: TransitionNone, reason: "awaiting_provider_verification"},
		{name: "engine cadence belongs to the accepted operation", status: "active", policy: models.CollectionPolicyEngine, end: rel(-100 * oneDay),
			charge: ChargeEvidence{PaymentOpenedCurrentPeriod: true}, watermark: true, want: TransitionNone, reason: "engine_collection_owned"},
		{name: "engine stalled dunning is not the sweep's to park", status: "past_due", policy: models.CollectionPolicyEngine, end: rel(-100 * oneDay), grace: rel(-time.Hour),
			want: TransitionNone, reason: "engine_collection_owned"},
		{name: "engine terminal charge evidence still cancels", status: "past_due", policy: models.CollectionPolicyEngine, end: rel(-100 * oneDay), grace: rel(-time.Hour),
			charge: ChargeEvidence{NonRetryableDecline: true, LastAttemptAt: decideNow}, want: TransitionCancel, certainty: collection.CertaintyNonRetryableDecline},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sub := SubscriptionState{Status: c.status, Rail: "nmi", HasPaymentMethod: !c.noPM, CollectionPolicy: models.CollectionPolicyProviderDunning,
				PeriodEnd: c.end, GraceEndsAt: c.grace, NextRetryScheduled: c.retry}
			if c.rail != "" {
				sub.Rail = c.rail
			}
			if c.policy != "" {
				sub.CollectionPolicy = c.policy
			}
			d := Decide(sub, EvidenceBundle{Charge: c.charge, WatermarkNewerThanPeriodEnd: c.watermark, EvidenceFloor: floor}, decideNow, 0)
			require.Equal(t, c.want, d.Kind, "reason=%q", d.Reason)
			if c.reason != "" {
				require.Equal(t, c.reason, d.Reason)
			}
			require.Equal(t, c.certainty, d.Certainty)
			require.Equal(t, c.floored, d.EvidenceFloored)
			if d.Kind == TransitionPastDue {
				require.Equal(t, c.end.Add(PeriodGrace), d.GraceEndsAt)
			}
		})
	}
}

// #821: NMI rebills forever, so a wedged next-billing date is dunning, never death.
func TestDecideStaleRosterDateNeverCancels(t *testing.T) {
	for _, staleDays := range []int{15, 30, 90, 365, 3 * 365} {
		end := rel(-time.Duration(staleDays) * oneDay)
		snap := &RemoteSnapshot{Provider: ProviderNMI, FetchedAt: decideNow, Coverage: SnapshotCoverage{SubscriptionsExhaustive: true},
			Subscriptions: []RemoteSubscription{*rosterSub(SubscriptionStatusPastDue, end)}}
		for _, status := range []string{"active", "past_due", "unknown"} {
			d := Decide(SubscriptionState{Status: status, Rail: "nmi", HasPaymentMethod: true, RailSubscriptionID: "rs", PeriodEnd: end},
				EvidenceBundle{Snapshot: snap}, decideNow, 0)
			require.Equal(t, TransitionParkUnknown, d.Kind, "stale %dd / %s: %s", staleDays, status, d.Reason)
		}
	}
}

// Properties over the whole input grid: no transition outruns its evidence.
func TestDecideInvariants(t *testing.T) {
	liveStatus := map[string]bool{"active": true, "past_due": true, "unknown": true}
	certainties := []string{collection.CertaintyProviderConfirmedDead, collection.CertaintyNonRetryableDecline, collection.CertaintyDunningExhausted}
	snap := func(p Provider, exhaustive bool, roster *RemoteSubscription, txns ...RemoteTransaction) *RemoteSnapshot {
		s := &RemoteSnapshot{Provider: p, FetchedAt: decideNow, Transactions: txns, Coverage: SnapshotCoverage{SubscriptionsExhaustive: exhaustive}}
		if roster != nil {
			s.Subscriptions = []RemoteSubscription{*roster}
		}
		return s
	}
	snaps := []*RemoteSnapshot{
		nil,
		snap(ProviderNMI, false, nil),
		snap(ProviderNMI, true, nil),
		snap(ProviderNMI, true, rosterSub(SubscriptionStatusActive, rel(20*oneDay))),
		snap(ProviderStripe, true, rosterSub(SubscriptionStatusActive, rel(20*oneDay))),
		snap(ProviderNMI, true, rosterSub(SubscriptionStatusPastDue, rel(-40*oneDay))),
		snap(ProviderNMI, true, rosterSub(SubscriptionStatusCancelled, nil)),
		snap(ProviderNMI, true, rosterSub(SubscriptionStatusActive, rel(20*oneDay)),
			rtx("rs", TransactionTypeDecline, false, decideNow.Add(-40*oneDay), "261"), rtx("rs", TransactionTypeDecline, false, decideNow.Add(-2*oneDay), "202")),
		snap(ProviderNMI, false, nil,
			rtx("rs", TransactionTypeSale, true, decideNow.Add(-9*oneDay), ""), rtx("rs", TransactionTypeDecline, false, decideNow.Add(-5*oneDay), "202")),
	}
	charges := []ChargeEvidence{
		{},
		{PaymentOpenedCurrentPeriod: true},
		{RenewalPaymentAfterPeriodEnd: true},
		{NonRetryableDecline: true, LastAttemptAt: decideNow.Add(-oneDay)},
		{NonRetryableDecline: true, LastAttemptAt: decideNow.Add(-30 * oneDay)},
		{RetryAttempts: 3, DunningMaxAttempts: 3},
	}
	times := []*time.Time{nil, rel(-90 * oneDay), rel(-10 * oneDay), rel(-time.Hour), rel(20 * oneDay)}

	for _, status := range []string{"active", "past_due", "unknown", "pending", "cancelled", "expired", "failed"} {
		for _, rail := range []string{"nmi", "stripe"} {
			for _, policy := range []models.CollectionPolicy{models.CollectionPolicyProviderDunning, models.CollectionPolicyEngine} {
				for _, end := range times {
					for _, grace := range []*time.Time{nil, rel(-time.Hour)} {
						for _, retry := range []bool{false, true} {
							for si, s := range snaps {
								for ci, charge := range charges {
									for _, floor := range []time.Time{{}, decideNow.Add(-7 * oneDay)} {
										for _, watermark := range []bool{false, true} {
											sub := SubscriptionState{Status: status, Rail: rail, HasPaymentMethod: true, CollectionPolicy: policy,
												RailSubscriptionID: "rs", PeriodEnd: end, GraceEndsAt: grace, NextRetryScheduled: retry}
											ev := EvidenceBundle{Snapshot: s, Charge: charge, WatermarkNewerThanPeriodEnd: watermark, EvidenceFloor: floor}
											d := Decide(sub, ev, decideNow, 0)
											where := func() string {
												return fmt.Sprintf("sub=%+v snap=%d charge=%d floor=%v watermark=%v -> %s/%s", sub, si, ci, floor, watermark, d.Kind, d.Reason)
											}

											if !liveStatus[status] && d.Kind != TransitionNone {
												t.Fatalf("terminal/pending row moved: %s", where())
											}
											if s == nil && charge == (ChargeEvidence{}) && !watermark && d.Kind != TransitionNone && d.Kind != TransitionParkUnknown {
												t.Fatalf("#664 evidence-less bundle acted: %s", where())
											}
											if s == nil && (d.Kind == TransitionRenew || d.Kind == TransitionAdoptPeriodEnd) {
												t.Fatalf("period moved without provider truth: %s", where())
											}
											if d.EvidenceFloored && d.Kind != TransitionParkUnknown {
												t.Fatalf("floored decision is not a park: %s", where())
											}
											if d.Kind != TransitionCancel && d.Certainty != "" {
												t.Fatalf("certainty on a non-cancel: %s", where())
											}
											switch d.Kind {
											case TransitionCancel:
												if !slices.Contains(certainties, d.Certainty) {
													t.Fatalf("#821 cancel without a named leg: %s", where())
												}
												if f := ev.evidenceFloor(); !f.IsZero() && (d.EvidenceAt.IsZero() || d.EvidenceAt.Before(f)) {
													t.Fatalf("#835 cancel evidence %v below floor %v: %s", d.EvidenceAt, f, where())
												}
												if s == nil && charge.certaintyLeg() == "" {
													t.Fatalf("first-party cancel without a certainty leg: %s", where())
												}
											case TransitionRenew:
												if !slices.ContainsFunc(s.Transactions, func(x RemoteTransaction) bool {
													return x.SubscriptionID == "rs" && x.Type == TransactionTypeSale && x.Success
												}) {
													t.Fatalf("renewed without a verified charge: %s", where())
												}
											case TransitionAdoptPeriodEnd:
												if d.NewPeriodEnd == nil || !d.NewPeriodEnd.After(decideNow) {
													t.Fatalf("adopted a non-future boundary: %s", where())
												}
											case TransitionPastDue:
												if d.GraceEndsAt.IsZero() {
													t.Fatalf("dunning entered without a grace marker: %s", where())
												}
											}
											if s != nil && len(s.Transactions) > 1 {
												rev := *s
												rev.Transactions = slices.Clone(s.Transactions)
												slices.Reverse(rev.Transactions)
												ev.Snapshot = &rev
												r := Decide(sub, ev, decideNow, 0)
												if r.Kind != d.Kind || r.Reason != d.Reason || r.Certainty != d.Certainty || !r.EvidenceAt.Equal(d.EvidenceAt) {
													t.Fatalf("transaction order changed the decision (%s/%s): %s", r.Kind, r.Reason, where())
												}
											}
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
}

func TestCancelChokepoint(t *testing.T) {
	base := Decision{Kind: TransitionCancel, Reason: "made_up"}

	got := gateCancelCertainty(base, EvidenceBundle{})
	require.Equal(t, TransitionParkUnknown, got.Kind, "an unnamed cancel cannot exist")
	require.Equal(t, "made_up_no_certainty", got.Reason)
	require.False(t, got.EvidenceFloored)

	gone := base
	gone.RemoteGone = true
	got = gateCancelCertainty(gone, EvidenceBundle{})
	require.Equal(t, TransitionCancel, got.Kind)
	require.Equal(t, collection.CertaintyProviderConfirmedDead, got.Certainty)

	got = gateCancelCertainty(base, EvidenceBundle{Charge: ChargeEvidence{NonRetryableDecline: true}})
	require.Equal(t, TransitionCancel, got.Kind)
	require.Equal(t, collection.CertaintyNonRetryableDecline, got.Certainty)

	for _, k := range []TransitionKind{TransitionNone, TransitionRenew, TransitionAdoptPeriodEnd, TransitionPastDue, TransitionParkUnknown} {
		d := Decision{Kind: k, Reason: "r"}
		require.Equal(t, d.Kind, gateCancelCertainty(d, EvidenceBundle{EvidenceFloor: decideNow}).Kind, "only cancels are gated")
	}
}

func TestAlignmentSlackNeverExceedsHalfThePeriod(t *testing.T) {
	end := decideNow
	for _, c := range []struct {
		start *time.Time
		end   *time.Time
		want  time.Duration
	}{
		{nil, &end, oneDay},
		{rel(-30 * oneDay), &end, oneDay},
		{rel(-oneDay), &end, 12 * time.Hour},
		{rel(-time.Hour), &end, 30 * time.Minute},
		{rel(oneDay), &end, oneDay},
		{&end, nil, oneDay},
	} {
		require.Equal(t, c.want, AlignmentSlack(c.start, c.end), "start=%v", c.start)
	}
}

func TestDecisionComponentsFollowTheOwningClock(t *testing.T) {
	now := time.Date(2041, time.March, 4, 5, 6, 7, 0, time.UTC)
	applier := NewDecisionApplier(nil, nil, clockwork.NewFakeClockAt(now))
	require.True(t, applier.clock.Now().Equal(now))
	require.True(t, applier.LC.Clock().Now().Equal(now))
	later := now.Add(48 * time.Hour)
	applier.SetClock(clockwork.NewFakeClockAt(later))
	require.True(t, applier.clock.Now().Equal(later))
	require.True(t, applier.LC.Clock().Now().Equal(later))

	zoned := time.Date(2044, time.May, 6, 7, 8, 9, 0, time.FixedZone("test", 2*60*60))
	store, writer := &PGStore{}, &PGLocalWriter{}
	(&Engine{Store: store, Writer: writer, Now: func() time.Time { return zoned }}).syncDBClocks()
	require.True(t, store.now().Equal(zoned))
	require.Equal(t, time.UTC, store.now().Location())
	require.True(t, writer.now().Equal(zoned))
}
