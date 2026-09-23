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

func TestFilterDelinquent(t *testing.T) {
	items := []DelinquentSubject{{Status: "past_due"}, {Status: "failed"}}
	require.Len(t, FilterDelinquent(items, "past_due"), 1)
	require.Len(t, FilterDelinquent(items, "all"), 2)
}
