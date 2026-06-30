package reconcile

import (
	"testing"
	"time"
)

// #632/#633: the pure unknown-resolution decision table, fixture-tested with
// hand-built provider snapshots (no DB, no provider calls).
func TestResolveUnknownFromSnapshot(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := now.Add(-10 * 24 * time.Hour) // lapsed 10d ago
	dunningWindow := 30 * 24 * time.Hour
	railSub := "rsub-1"

	sale := func(success bool, at time.Time) RemoteTransaction {
		return RemoteTransaction{TransactionID: "tx-" + at.Format("0102150405"), SubscriptionID: railSub, Type: TransactionTypeSale, Success: success, AmountCents: 5000, Currency: "usd", OccurredAt: at}
	}
	decline := func(at time.Time) RemoteTransaction {
		return RemoteTransaction{TransactionID: "txd-" + at.Format("0102150405"), SubscriptionID: railSub, Type: TransactionTypeDecline, Success: false, OccurredAt: at}
	}
	roster := func(status SubscriptionStatus, next *time.Time) RemoteSubscription {
		return RemoteSubscription{RailSubscriptionID: railSub, Status: status, NextBillingAt: next}
	}
	nextEnd := now.Add(20 * 24 * time.Hour)

	cases := []struct {
		name         string
		snap         *RemoteSnapshot
		wantOutcome  UnknownOutcome
		wantNewEnd   *time.Time
		wantBackfill int
	}{
		{
			name:        "nil snapshot → unreachable",
			snap:        nil,
			wantOutcome: UnknownOutcomeUnreachable,
		},
		{
			name:         "successful renewal charge → renewed + backfill, advance to next billing",
			snap:         &RemoteSnapshot{Transactions: []RemoteTransaction{sale(true, periodEnd.Add(time.Hour))}, Subscriptions: []RemoteSubscription{roster(SubscriptionStatusActive, &nextEnd)}},
			wantOutcome:  UnknownOutcomeRenewed,
			wantNewEnd:   &nextEnd,
			wantBackfill: 1,
		},
		{
			name:        "roster active, no charge in window → renewed (provider confirms current)",
			snap:        &RemoteSnapshot{Subscriptions: []RemoteSubscription{roster(SubscriptionStatusActive, &nextEnd)}},
			wantOutcome: UnknownOutcomeRenewed,
			wantNewEnd:  &nextEnd,
		},
		{
			name:         "declined renewal within dunning window → past_due + backfill the decline",
			snap:         &RemoteSnapshot{Transactions: []RemoteTransaction{decline(periodEnd.Add(time.Hour))}, Subscriptions: []RemoteSubscription{roster(SubscriptionStatusPastDue, nil)}},
			wantOutcome:  UnknownOutcomePastDue,
			wantBackfill: 1,
		},
		{
			name:         "declined renewal older than dunning window → cancelled",
			snap:         &RemoteSnapshot{Transactions: []RemoteTransaction{decline(periodEnd.Add(time.Hour))}},
			wantOutcome:  UnknownOutcomeCancelled,
			wantBackfill: 1,
			// periodEnd is 10d ago; shrink the window so the lapse is "too old".
		},
		{
			name:        "roster cancelled → cancelled",
			snap:        &RemoteSnapshot{Subscriptions: []RemoteSubscription{roster(SubscriptionStatusCancelled, nil)}},
			wantOutcome: UnknownOutcomeCancelled,
		},
		{
			name:        "absent from exhaustive roster → cancelled (deleted at provider)",
			snap:        &RemoteSnapshot{Subscriptions: []RemoteSubscription{}, Coverage: SnapshotCoverage{SubscriptionsExhaustive: true}},
			wantOutcome: UnknownOutcomeCancelled,
		},
		{
			name:        "absent from NON-exhaustive pull → unreachable (stay unknown)",
			snap:        &RemoteSnapshot{Subscriptions: []RemoteSubscription{}},
			wantOutcome: UnknownOutcomeUnreachable,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			window := dunningWindow
			if c.name == "declined renewal older than dunning window → cancelled" {
				window = 24 * time.Hour // lapse (10d) now exceeds the window
			}
			v := ResolveUnknownFromSnapshot(UnknownSubject{RailSubscriptionID: railSub, PeriodEnd: periodEnd}, c.snap, now, window)
			if v.Outcome != c.wantOutcome {
				t.Fatalf("outcome = %v, want %v", v.Outcome, c.wantOutcome)
			}
			if (v.NewPeriodEnd == nil) != (c.wantNewEnd == nil) {
				t.Fatalf("newPeriodEnd nil-ness mismatch: got %v want %v", v.NewPeriodEnd, c.wantNewEnd)
			}
			if c.wantNewEnd != nil && !v.NewPeriodEnd.Equal(*c.wantNewEnd) {
				t.Fatalf("newPeriodEnd = %v, want %v", v.NewPeriodEnd, c.wantNewEnd)
			}
			if len(v.Backfill) != c.wantBackfill {
				t.Fatalf("backfill = %d, want %d", len(v.Backfill), c.wantBackfill)
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
