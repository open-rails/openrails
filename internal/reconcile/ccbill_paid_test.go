package reconcile_test

import (
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/reconcile"
)

// A CCBill cancellation naming no paid-through keeps the period a rebill
// read with it paid, unless a refund or chargeback in the read reverses it.
func TestCCBillCancellationKeepsOnlyUnreversedRebills(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	start, end := now.Add(-30*24*time.Hour), now.Add(-12*time.Hour)
	decide := func(reversal ...reconcile.RemoteTransaction) reconcile.Decision {
		txns := append([]reconcile.RemoteTransaction{{TransactionID: "0912233445500000001", SubscriptionID: "0223344556600000001",
			Type: reconcile.TransactionTypeSale, Success: true, Amount: "10.00", OccurredAt: end}}, reversal...)
		return reconcile.Decide(reconcile.SubscriptionState{Status: "active", Rail: "ccbill", RailSubscriptionID: "0223344556600000001", PeriodStart: &start, PeriodEnd: &end},
			reconcile.EvidenceBundle{Snapshot: &reconcile.RemoteSnapshot{Provider: reconcile.ProviderCCBill, FetchedAt: now,
				Subscriptions: []reconcile.RemoteSubscription{{RailSubscriptionID: "0223344556600000001", Status: reconcile.SubscriptionStatusCanceled, RawStatus: "CANCELLATION"}},
				Transactions:  txns}}, now, 0)
	}
	if d := decide(); d.Kind != reconcile.TransitionCancel || !d.KeepsPaidTime || d.PaidThrough == nil || !d.PaidThrough.After(now) {
		t.Fatalf("a rebill read with the cancellation = %+v", d)
	}
	for _, kind := range []reconcile.TransactionType{reconcile.TransactionTypeRefund, reconcile.TransactionTypeChargeback} {
		d := decide(reconcile.RemoteTransaction{SubscriptionID: "0223344556600000001", Type: kind, Success: true, Amount: "-10.00", OccurredAt: end.Add(time.Hour)})
		if d.Kind != reconcile.TransitionCancel || d.PaidThrough != nil {
			t.Fatalf("a rebill its %s reversed paid through %v", kind, d.PaidThrough)
		}
	}
	if d := decide(reconcile.RemoteTransaction{SubscriptionID: "0223344556600000001", Type: reconcile.TransactionTypeRefund, Success: true, Amount: "2.00", OccurredAt: end.Add(time.Hour)}); d.PaidThrough == nil {
		t.Fatal("a partial refund reversed the whole rebill")
	}
}
