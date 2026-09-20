package reconcile

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeFetcher struct {
	provider Provider
	snap     *RemoteSnapshot
	err      error
}

func (f *fakeFetcher) Name() string { return string(f.provider) }
func (f *fakeFetcher) Capabilities() Capabilities {
	if f.snap != nil {
		return f.snap.Capabilities
	}
	return Capabilities{}
}
func (f *fakeFetcher) Fetch(ctx context.Context, params FetchParams) (*RemoteSnapshot, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.snap, nil
}

func tp(t time.Time) *time.Time { return &t }

var testNow = time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)

func liveLocalSub(provider Provider, psid string) LocalSubscription {
	priceID := uuid.New()
	return LocalSubscription{
		ID:                    uuid.New(),
		CustomerID:            uuid.New(),
		PriceID:               &priceID,
		ProductID:             uuid.New(),
		Status:                "active",
		Rail:                  string(provider),
		RailSubscriptionID:    psid,
		CurrentPeriodStartsAt: tp(testNow.Add(-10 * 24 * time.Hour)),
		CurrentPeriodEndsAt:   tp(testNow.Add(20 * 24 * time.Hour)),
		StartedAt:             testNow.Add(-100 * 24 * time.Hour),
		EntitlementNames:      []string{"premium"},
	}
}

func nmiSaleTxn(txnID, orderID string, success bool) RemoteTransaction {
	t := RemoteTransaction{
		TransactionID: txnID,
		Type:          TransactionTypeSale,
		Success:       success,
		AmountCents:   999,
		Currency:      "USD",
		OccurredAt:    testNow.Add(-48 * time.Hour),
		Raw:           rawJSON(map[string]any{"order_id": orderID}),
	}
	if !success {
		t.Type = TransactionTypeDecline
		t.DeclineReason = "Insufficient funds"
	}
	return t
}

func findByType(findings []FindingRecord, t FindingType) []FindingRecord {
	var out []FindingRecord
	for _, f := range findings {
		if f.Type == t {
			out = append(out, f)
		}
	}
	return out
}

func TestDunningForensics(t *testing.T) {
	state := LocalState{}
	never := liveLocalSub(ProviderNMI, "nmi-never")
	never.Status = "past_due"
	exhausted := liveLocalSub(ProviderNMI, "nmi-exhausted")
	exhausted.Status = "cancelled"
	exhausted.CancelType = "expired"
	exhausted.RetryAttempts = 3
	exhausted.LastRetryAt = tp(testNow.Add(-40 * 24 * time.Hour))
	state.Subscriptions = []LocalSubscription{never, exhausted}

	snap := &RemoteSnapshot{
		Provider:     ProviderNMI,
		Capabilities: Capabilities{Subscriptions: true, Transactions: true},
		Transactions: []RemoteTransaction{
			nmiSaleTxn("d1", fmt.Sprintf("rebill-%s-%d", never.ID, testNow.Unix()), false),
			nmiSaleTxn("d2", fmt.Sprintf("rebill-%s-%d", never.ID, testNow.Unix()), false),
			nmiSaleTxn("d3", fmt.Sprintf("rebill-%s-%d", exhausted.ID, testNow.Unix()), false),
		},
	}
	report := computeDunningForensics(ProviderNMI, snap, &state, nil, "not configured", testNow)
	require.NotNil(t, report)
	assert.Equal(t, 2, report.SubscriptionsExamined)
	assert.Equal(t, 1, report.NeverAttempted)
	assert.Equal(t, 1, report.AttemptedExhausted)
	assert.Equal(t, map[string]int{"Insufficient funds": 3}, report.DeclineReasons)
	require.NotNil(t, report.LastLocalDunningAction)
	assert.True(t, report.LastLocalDunningAction.Equal(*exhausted.LastRetryAt))
	require.Len(t, report.Details, 2)
}

func TestParseRebillOrderID(t *testing.T) {
	id := uuid.New()
	got, ok := parseRebillOrderID(fmt.Sprintf("rebill-%s-1765432100", id))
	require.True(t, ok)
	assert.Equal(t, id, got)

	got, ok = parseRebillOrderID(id.String())
	require.True(t, ok)
	assert.Equal(t, id, got)

	_, ok = parseRebillOrderID("rebill-not-a-uuid-123")
	assert.False(t, ok)
	_, ok = parseRebillOrderID("")
	assert.False(t, ok)
	_, ok = parseRebillOrderID("upgrade-12345678-87654321")
	assert.False(t, ok)
}

func TestApplyActionsCarryThePullsPSP(t *testing.T) {
	psp := uuid.New()
	findings := []Finding{
		{Apply: &ApplyAction{BackfillPayment: &BackfillPaymentAction{TransactionID: "txn-1"}}},
		{Apply: &ApplyAction{RecordRefund: &RecordRefundAction{TransactionID: "re-1"}}},
		{Apply: &ApplyAction{Materialize: &MaterializeSubscriptionAction{
			RailSubscriptionID: "sub-1",
			Backfill:           &BackfillPaymentAction{TransactionID: "txn-2"},
		}}},
	}
	bindApplyActions(findings, psp)
	require.NotNil(t, findings[0].Apply.BackfillPayment.PspID)
	assert.Equal(t, psp, *findings[0].Apply.BackfillPayment.PspID)
	require.NotNil(t, findings[1].Apply.RecordRefund.PspID)
	assert.Equal(t, psp, *findings[1].Apply.RecordRefund.PspID)
	assert.Equal(t, psp, findings[2].Apply.Materialize.PspID)
	require.NotNil(t, findings[2].Apply.Materialize.Backfill.PspID)
	assert.Equal(t, psp, *findings[2].Apply.Materialize.Backfill.PspID)
}

// The display-only aggregation accepts history from more than the current PG
// failed-payment source. Keep local-ID/rail-ID correlation, deep timestamps and
// mixed event kinds as a pure table calculation; it needs no imitation database.
func TestDunningHistoryAggregation(t *testing.T) {
	sub := liveLocalSub(ProviderNMI, "nmi-hist")
	sub.Status = "past_due"
	state := LocalState{Subscriptions: []LocalSubscription{sub}}
	snap := &RemoteSnapshot{Provider: ProviderNMI, Capabilities: Capabilities{Subscriptions: true, Transactions: true},
		Subscriptions: []RemoteSubscription{{RailSubscriptionID: "nmi-hist", Status: SubscriptionStatusPastDue, NextBillingAt: sub.CurrentPeriodEndsAt}}}
	histAt := testNow.Add(-200 * 24 * time.Hour)
	history := []HistoryEvent{
		{Table: "payment_events", EventType: "charge_failed", Rail: "nmi", SubscriptionID: &sub.ID, OccurredAt: histAt},
		{Table: "payment_events", EventType: "charge_success", Rail: "nmi", SubscriptionID: &sub.ID, OccurredAt: histAt.Add(-30 * 24 * time.Hour)},
		{Table: "subscription_events", EventType: "subscription_cancelled", Rail: "nmi", RailSubscriptionID: "nmi-hist", OccurredAt: histAt.Add(24 * time.Hour)},
		{Table: "payment_events", EventType: "charge_failed", Rail: "nmi", OccurredAt: histAt},
	}
	report := computeDunningForensics(ProviderNMI, snap, &state, history, "ok: 4 events", testNow)
	require.NotNil(t, report)
	require.Equal(t, "ok: 4 events (3 correlated)", report.HistorySource)
	require.Len(t, report.Details, 1)
	line := report.Details[0]
	require.Equal(t, 3, line.HistoryEvents)
	require.Equal(t, 1, line.HistoryFailures)
	require.Equal(t, 1, line.HistorySuccesses)
	var sources, kinds []string
	for _, event := range line.Timeline {
		sources = append(sources, event.Source)
		kinds = append(kinds, event.Kind)
	}
	require.Contains(t, sources, "history")
	require.Contains(t, kinds, "charge_failed")
	require.Contains(t, kinds, "subscription_cancelled")
	require.NotNil(t, report.LastDunningActionAnySource)
	require.Equal(t, histAt, *report.LastDunningActionAnySource)
	require.Equal(t, "history", report.LastDunningActionVia)
	require.Equal(t, "never_attempted", line.Classification)
	require.Equal(t, 1, report.NeverAttempted)
}
