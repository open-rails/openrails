package reconcile

import (
	"fmt"
	"testing"
	"time"
)

// #665 property: for a FIXED evidence bundle, ANY interleaving/ordering of
// plane execution yields the same terminal state — the #664 acceptance
// ("scheduler ordering cannot change outcomes"), generalized from tested
// behavior to a structural guarantee over the ONE decider.
//
// The model mirrors the production planes exactly:
//   - LIFE sweep: Decide over first-party charge/watermark evidence (no
//     snapshot), for active/past_due rows (the lapsed-cohort scan).
//   - PULL: Decide over the snapshot slice, invoked only for rows the diff
//     flags divergent (remote dead / absent-from-exhaustive / status differs /
//     period drift > 48h) — the mirror-writer's decider invocation.
//   - UNKNOWN resolution: Decide over the snapshot, for `unknown` rows.
//
// Transitions apply through a model of ApplyDecision's guards (the `unknown`
// waypoint + lifecycle preconditions).

type modelSub struct {
	status             string
	rail               string
	vaulted            bool
	railSubID          string
	periodStart        *time.Time
	periodEnd          *time.Time
	graceEndsAt        *time.Time
	nextRetryScheduled bool
}

func (m modelSub) state() SubscriptionState {
	return SubscriptionState{
		Status: m.status, Rail: m.rail, Vaulted: m.vaulted,
		RailSubscriptionID: m.railSubID, PeriodEnd: m.periodEnd,
		GraceEndsAt: m.graceEndsAt, NextRetryScheduled: m.nextRetryScheduled,
	}
}

// applyToModel mirrors ApplyDecision + the lifecycle chokepoints' semantics.
func (m *modelSub) applyToModel(d Decision) bool {
	transitional := m.status == "active" || m.status == "past_due" || m.status == "unknown"
	park := func() bool {
		if m.status != "active" && m.status != "past_due" {
			return false
		}
		m.status = "unknown"
		m.graceEndsAt = nil
		m.nextRetryScheduled = false
		return true
	}
	switch d.Kind {
	case TransitionParkUnknown:
		return park()
	case TransitionPastDue:
		if m.status == "unknown" {
			m.status = "past_due"
			if m.graceEndsAt == nil {
				g := d.GraceEndsAt
				m.graceEndsAt = &g
			}
			return true
		}
		if m.status != "active" || m.periodEnd == nil {
			return false
		}
		m.status = "past_due"
		if m.graceEndsAt == nil {
			g := d.GraceEndsAt
			m.graceEndsAt = &g
		}
		return true
	case TransitionRenew, TransitionAdoptPeriodEnd, TransitionCancel:
		if m.status != "unknown" {
			if !transitional {
				return false
			}
			if !park() {
				return false
			}
		}
		switch d.Kind {
		case TransitionRenew:
			m.status = "active"
			if d.NewPeriodEnd != nil {
				start := m.periodEnd
				if start != nil && d.NewPeriodEnd.After(*start) {
					m.periodStart = start
					e := *d.NewPeriodEnd
					m.periodEnd = &e
				}
			}
			m.nextRetryScheduled = false
		case TransitionAdoptPeriodEnd:
			m.status = "active"
			if d.NewPeriodEnd != nil {
				e := *d.NewPeriodEnd
				m.periodEnd = &e
			}
			m.nextRetryScheduled = false
		case TransitionCancel:
			m.status = "cancelled"
			m.graceEndsAt = nil
			m.nextRetryScheduled = false
		}
		return true
	default:
		return false
	}
}

// The three planes, each presenting its slice of the FIXED bundle.
type plane func(m *modelSub, ev EvidenceBundle, now time.Time) bool

func lifePlane(m *modelSub, ev EvidenceBundle, now time.Time) bool {
	if m.status != "active" && m.status != "past_due" {
		return false
	}
	d := Decide(m.state(), EvidenceBundle{Charge: ev.Charge, WatermarkNewerThanPeriodEnd: ev.WatermarkNewerThanPeriodEnd}, now, 0)
	return m.applyToModel(d)
}

func pullPlane(m *modelSub, ev EvidenceBundle, now time.Time) bool {
	if ev.Snapshot == nil || m.railSubID == "" {
		return false
	}
	if m.status != "active" && m.status != "past_due" && m.status != "pending" {
		return false // the diff only routes LIVE local rows to the decider
	}
	var remote *RemoteSubscription
	for i := range ev.Snapshot.Subscriptions {
		if ev.Snapshot.Subscriptions[i].RailSubscriptionID == m.railSubID {
			remote = &ev.Snapshot.Subscriptions[i]
			break
		}
	}
	// The diff's divergence pre-filter.
	divergent := false
	switch {
	case remote == nil:
		divergent = ev.Snapshot.Coverage.SubscriptionsExhaustive
	case remote.Status == SubscriptionStatusUnknown:
		divergent = false
	case remote.Status == SubscriptionStatusCancelled || remote.Status == SubscriptionStatusExpired:
		divergent = true
	case string(remote.Status) != m.status:
		divergent = true
	case remote.NextBillingAt != nil && m.periodEnd != nil:
		drift := remote.NextBillingAt.Sub(*m.periodEnd)
		if drift < 0 {
			drift = -drift
		}
		divergent = drift > 48*time.Hour
	}
	if !divergent {
		return false
	}
	d := Decide(m.state(), EvidenceBundle{Snapshot: ev.Snapshot}, now, 0)
	return m.applyToModel(d)
}

func unknownPlane(m *modelSub, ev EvidenceBundle, now time.Time) bool {
	if m.status != "unknown" || ev.Snapshot == nil {
		return false
	}
	d := Decide(m.state(), EvidenceBundle{Snapshot: ev.Snapshot}, now, 0)
	return m.applyToModel(d)
}

func terminalKey(m modelSub) string {
	pe := "nil"
	if m.periodEnd != nil {
		pe = m.periodEnd.UTC().Format(time.RFC3339)
	}
	g := "nil"
	if m.graceEndsAt != nil {
		g = m.graceEndsAt.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("%s|end=%s|grace=%s", m.status, pe, g)
}

func TestDecide_PlaneOrderingCannotChangeOutcomes(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	lapsed := now.Add(-10 * 24 * time.Hour)
	start := lapsed.Add(-30 * 24 * time.Hour)
	nextEnd := now.Add(20 * 24 * time.Hour)
	graceGone := now.Add(-3 * 24 * time.Hour)

	const rs = "rs-prop"
	roster := func(status SubscriptionStatus, next *time.Time, exhaustive bool) *RemoteSnapshot {
		s := &RemoteSnapshot{Provider: ProviderNMI, Coverage: SnapshotCoverage{SubscriptionsExhaustive: exhaustive}}
		if status != "" {
			s.Subscriptions = []RemoteSubscription{{RailSubscriptionID: rs, Status: status, NextBillingAt: next}}
		}
		return s
	}
	withTxn := func(s *RemoteSnapshot, txnType TransactionType, success bool, at time.Time) *RemoteSnapshot {
		s.Transactions = append(s.Transactions, RemoteTransaction{
			TransactionID: "tx-prop", SubscriptionID: rs, Type: txnType, Success: success,
			AmountCents: 999, Currency: "USD", OccurredAt: at,
		})
		return s
	}

	activeLapsed := modelSub{status: "active", rail: "nmi", vaulted: true, railSubID: rs, periodStart: &start, periodEnd: &lapsed}
	activeLapsedVaultless := modelSub{status: "active", rail: "nmi", vaulted: false, railSubID: rs, periodStart: &start, periodEnd: &lapsed}
	pastDueStalled := modelSub{status: "past_due", rail: "nmi", vaulted: true, railSubID: rs, periodStart: &start, periodEnd: &lapsed, graceEndsAt: &graceGone}
	unknownParked := modelSub{status: "unknown", rail: "nmi", vaulted: true, railSubID: rs, periodStart: &start, periodEnd: &lapsed}

	fixtures := []struct {
		name    string
		initial modelSub
		ev      EvidenceBundle
	}{
		{"no evidence at all (the #664 imported shape)", activeLapsed, EvidenceBundle{}},
		{"ownership evidence only (payment opened period)", activeLapsed,
			EvidenceBundle{Charge: ChargeEvidence{PaymentOpenedCurrentPeriod: true}}},
		{"watermark freshness only", activeLapsed, EvidenceBundle{WatermarkNewerThanPeriodEnd: true}},
		{"renewal payment recorded locally, no snapshot", activeLapsed,
			EvidenceBundle{Charge: ChargeEvidence{RenewalPaymentAfterPeriodEnd: true}}},
		{"snapshot: verified renewal charge + roster active future end", activeLapsed,
			EvidenceBundle{Snapshot: withTxn(roster(SubscriptionStatusActive, &nextEnd, true), TransactionTypeSale, true, lapsed.Add(time.Hour))}},
		{"snapshot: roster alive future end, no charge (adopt)", activeLapsed,
			EvidenceBundle{Snapshot: roster(SubscriptionStatusActive, &nextEnd, true)}},
		{"snapshot: roster dead (provider-confirmed)", activeLapsed,
			EvidenceBundle{Snapshot: roster(SubscriptionStatusCancelled, nil, true)}},
		{"snapshot: roster dead + renewal charge (charge cannot resurrect)", activeLapsed,
			EvidenceBundle{Snapshot: withTxn(roster(SubscriptionStatusCancelled, nil, true), TransactionTypeSale, true, lapsed.Add(time.Hour))}},
		{"snapshot: absent from exhaustive roster", activeLapsed,
			EvidenceBundle{Snapshot: roster("", nil, true)}},
		{"snapshot: absent from NON-exhaustive pull (proves nothing)", activeLapsed,
			EvidenceBundle{Snapshot: roster("", nil, false)}},
		{"snapshot: decline within window + ownership evidence", activeLapsed,
			EvidenceBundle{
				Snapshot: withTxn(roster(SubscriptionStatusPastDue, nil, true), TransactionTypeDecline, false, lapsed.Add(time.Hour)),
				Charge:   ChargeEvidence{PaymentOpenedCurrentPeriod: true},
			}},
		{"snapshot: roster dead + vault-less local (rail heuristic irrelevant)", activeLapsedVaultless,
			EvidenceBundle{Snapshot: roster(SubscriptionStatusExpired, nil, true)}},
		{"stalled past_due, no evidence", pastDueStalled, EvidenceBundle{}},
		{"stalled past_due + snapshot renewal charge", pastDueStalled,
			EvidenceBundle{Snapshot: withTxn(roster(SubscriptionStatusActive, &nextEnd, true), TransactionTypeSale, true, lapsed.Add(time.Hour))}},
		{"parked unknown + roster dead", unknownParked,
			EvidenceBundle{Snapshot: roster(SubscriptionStatusCancelled, nil, true)}},
		{"parked unknown + no snapshot (stays parked)", unknownParked, EvidenceBundle{}},
	}

	planes := []plane{lifePlane, pullPlane, unknownPlane}

	// Every plane sequence of length 5 over {LIFE, PULL, UNKNOWN} (3^5 = 243
	// interleavings incl. repeats), then run all planes to fixpoint so partial
	// schedules can't masquerade as different terminals.
	var sequences [][]int
	var build func(prefix []int)
	build = func(prefix []int) {
		if len(prefix) == 5 {
			sequences = append(sequences, append([]int(nil), prefix...))
			return
		}
		for i := range planes {
			build(append(prefix, i))
		}
	}
	build(nil)

	for _, fx := range fixtures {
		t.Run(fx.name, func(t *testing.T) {
			terminals := map[string][]string{}
			for _, seq := range sequences {
				m := fx.initial
				for _, pi := range seq {
					planes[pi](&m, fx.ev, now)
				}
				// Fixpoint: cycle every plane until nothing moves (bounded).
				for round := 0; round < 6; round++ {
					moved := false
					for _, p := range planes {
						if p(&m, fx.ev, now) {
							moved = true
						}
					}
					if !moved {
						break
					}
				}
				key := terminalKey(m)
				terminals[key] = append(terminals[key], fmt.Sprint(seq))
			}
			if len(terminals) != 1 {
				for k, seqs := range terminals {
					t.Errorf("terminal %q reached by %d sequences (e.g. %s)", k, len(seqs), seqs[0])
				}
				t.Fatal("#665 violated: plane ordering changed the terminal state for a fixed evidence bundle")
			}
		})
	}
}

// #664, structural: an evidence-less bundle can only PARK — for EVERY
// subscription state, Decide without provider snapshot, charge evidence,
// watermark freshness, or certainty legs never cancels, never renews, never
// extends access, never enters dunning.
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
								Status: status, Rail: rail, Vaulted: vaulted,
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
