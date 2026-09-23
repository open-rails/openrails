package billinganalysis

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDeriveClassifiesSourceAndCarriesOpenCases(t *testing.T) {
	loc := time.UTC
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	events := []evidenceEvent{
		{Provider: "provider-a", PSPID: "psp", EventKey: "failed", TransactionID: "f", SubscriptionRef: "sub", Type: "decline", Source: "recurring", Success: false, AmountCents: 1000, Currency: "USD", OccurredAt: base, DeclineCode: "05", DeclineReason: "declined"},
		{Provider: "provider-a", PSPID: "psp", EventKey: "sale", TransactionID: "s", SubscriptionRef: "sub", Type: "sale", Source: "recurring", Success: true, AmountCents: 1000, Currency: "USD", OccurredAt: base.Add(48 * time.Hour)},
		{Provider: "provider-a", PSPID: "psp", EventKey: "signup", TransactionID: "n", SubscriptionRef: "new", Type: "sale", Source: "api", Success: true, AmountCents: 500, Currency: "USD", OccurredAt: base.Add(24 * time.Hour)},
	}
	report := derive(events, nil, Options{From: base.Add(-time.Hour), To: base.Add(72 * time.Hour), Location: loc})
	require.Len(t, report.Days, 4)
	require.Equal(t, 1, report.Days[0].FailedRebills)
	require.Equal(t, 1, report.Days[1].SettledSignups)
	require.Equal(t, 1, report.Days[1].OpenUnbilledUsers, "the failure remains open until a later success")
	require.Equal(t, 1, report.Days[2].SettledRebills)
	require.Empty(t, report.Days[2].Unbilled)
	require.Len(t, report.Failures, 1)
	require.Equal(t, "recurring", report.Failures[0].Source)
}

func TestDeriveKeepsFailureOpenUntilSuccess(t *testing.T) {
	loc := time.UTC
	base := time.Date(2026, 2, 1, 1, 0, 0, 0, time.UTC)
	events := []evidenceEvent{{Provider: "p", PSPID: "a", EventKey: "f", SubscriptionRef: "sub", Type: "decline", OccurredAt: base, Success: false, AmountCents: 1}}
	report := derive(events, nil, Options{From: base, To: base.Add(2 * 24 * time.Hour), Location: loc})
	require.Equal(t, 1, report.Days[0].OpenUnbilledUsers)
	require.Equal(t, 1, report.Days[1].OpenUnbilledUsers)
	require.Equal(t, 1, report.Days[2].OpenUnbilledUsers)
	require.Equal(t, "p/a/sub", report.Days[2].Unbilled[0].ObligationKey)
}

func TestDeriveSortsEventsAndTreatsAPIRecoveryAsSignup(t *testing.T) {
	base := time.Date(2026, 3, 1, 1, 0, 0, 0, time.UTC)
	// Deliberately provide the success before the initial failed API attempt.
	events := []evidenceEvent{
		{Provider: "p", PSPID: "a", EventKey: "success", SubscriptionRef: "sub", Type: "sale", Source: "api", Success: true, AmountCents: 100, Currency: "USD", OccurredAt: base.Add(time.Hour)},
		{Provider: "p", PSPID: "a", EventKey: "retry", SubscriptionRef: "sub", Type: "decline", Source: "api", Success: false, AmountCents: 100, Currency: "USD", OccurredAt: base.Add(-time.Hour)},
	}
	report := derive(events, nil, Options{From: base.Add(-2 * time.Hour), To: base.Add(2 * time.Hour), Location: time.UTC})
	require.Equal(t, 1, report.Days[1].FailedSignups)
	require.Equal(t, 1, report.Days[1].SettledSignups)
	require.Empty(t, report.Days[1].Unbilled)
}

func TestDeriveDeduplicatesRosterAndOpenFailure(t *testing.T) {
	base := time.Date(2026, 6, 1, 1, 0, 0, 0, time.UTC)
	events := []evidenceEvent{{Provider: "p", PSPID: "a", EventKey: "f", SubscriptionRef: "sub", Type: "decline", Source: "recurring", OccurredAt: base, Success: false, AmountCents: 100}}
	subs := []subscriptionObservation{{Provider: "p", PSPID: "a", SubscriptionRef: "sub", Status: "past_due"}}
	report := derive(events, subs, Options{From: base, To: base, Location: time.UTC})
	require.Len(t, report.CurrentDelinquent, 1)
	require.Equal(t, "past_due", report.CurrentDelinquent[0].Status)
}

func TestClassifyDoesNotAssumeLaterAPIIsRebill(t *testing.T) {
	e := evidenceEvent{Source: "api", SubscriptionRef: "sub"}
	require.Equal(t, "signup", classify(e, classificationState{}))
	require.Equal(t, "other", classify(e, classificationState{seen: true, successful: true}))
	require.Equal(t, "rebill", classify(evidenceEvent{Source: "recurring", SubscriptionRef: "sub"}, classificationState{successful: true}))
}

func TestDeriveResetsFailureCountAfterSettlement(t *testing.T) {
	base := time.Date(2026, 5, 1, 1, 0, 0, 0, time.UTC)
	events := []evidenceEvent{
		{Provider: "p", PSPID: "a", EventKey: "f1", SubscriptionRef: "sub", Type: "decline", Source: "recurring", OccurredAt: base, Success: false, AmountCents: 100},
		{Provider: "p", PSPID: "a", EventKey: "ok", SubscriptionRef: "sub", Type: "sale", Source: "recurring", OccurredAt: base.Add(24 * time.Hour), Success: true, AmountCents: 100},
		{Provider: "p", PSPID: "a", EventKey: "f2", SubscriptionRef: "sub", Type: "decline", Source: "recurring", OccurredAt: base.Add(48 * time.Hour), Success: false, AmountCents: 100},
	}
	report := derive(events, nil, Options{From: base, To: base.Add(48 * time.Hour), Location: time.UTC})
	require.Len(t, report.Failures, 2)
	require.Equal(t, 1, report.Failures[0].FailureCount)
	require.Equal(t, 1, report.Failures[1].FailureCount)
}

func TestDeriveDoesNotProjectCurrentRosterIntoHistoricalDays(t *testing.T) {
	base := time.Date(2026, 4, 1, 1, 0, 0, 0, time.UTC)
	subs := []subscriptionObservation{{Provider: "p", PSPID: "a", SubscriptionRef: "sub", Status: "past_due"}}
	report := derive(nil, subs, Options{From: base, To: base.Add(24 * time.Hour), Location: time.UTC})
	require.Equal(t, 0, report.Days[0].DelinquentUsers)
	require.Equal(t, 1, report.Days[1].DelinquentUsers)
	require.Len(t, report.CurrentDelinquent, 1)
}

func TestFilterDelinquent(t *testing.T) {
	items := []DelinquentSubject{{Status: "past_due"}, {Status: "failed"}}
	require.Len(t, FilterDelinquent(items, "past_due"), 1)
	require.Len(t, FilterDelinquent(items, "all"), 2)
}
