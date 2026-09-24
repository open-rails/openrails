package hosttools

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/reconcile"
)

func TestPullProviderWindowParsing(t *testing.T) {
	for input, want := range map[string]time.Time{
		"":                          {},
		" 2026-09-01 ":              time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		"2026-09-01T10:30:00+02:00": time.Date(2026, 9, 1, 8, 30, 0, 0, time.UTC),
	} {
		got, err := parsePullProviderTime(input, "since")
		require.NoError(t, err, input)
		require.True(t, want.Equal(got), "%q: %v", input, got)
	}
	for _, input := range []string{"yesterday", "2026-13-01", "2026-09-01 10:30"} {
		_, err := parsePullProviderTime(input, "until")
		require.ErrorContains(t, err, "invalid --until", input)
	}
}

// The run log is line-oriented key=value: provider-supplied values cannot
// forge fields or lines, and empty fields are omitted.
func TestPullProviderLogLinesCannotBeForged(t *testing.T) {
	var buf bytes.Buffer
	ts := time.Date(2026, 9, 1, 0, 0, 0, 0, time.FixedZone("x", 3600))
	writeLogLine(&buf, ts, lf("event", "finding"), lf("empty", ""), lf("subject_key", "sub_1 status=ok\nevent=forged"), lf("plain", "a-b_c"))
	line := buf.String()
	require.Equal(t, 1, strings.Count(line, "\n"))
	require.Equal(t, `ts=2026-08-31T23:00:00Z event=finding subject_key="sub_1 status=ok\nevent=forged" plain=a-b_c`+"\n", line)
	require.Equal(t, `""`, quoteLogValue(""))
	require.Equal(t, `"a=b"`, quoteLogValue("a=b"))
}

func TestPullProviderSummaries(t *testing.T) {
	planned := []reconcile.MutationRecord{
		{Table: "subscriptions", Operation: "insert", RowsAffected: 0},
		{Table: "subscriptions", Operation: "insert", RowsAffected: 2},
		{Table: "payments", Operation: "update", RowsAffected: 1},
		{Table: "", Operation: "insert"},
		{Table: "payments", Operation: ""},
	}
	counts := summarizeMutations(planned, nil)
	require.Equal(t, "payments updated=1; subscriptions inserted=3", formatMutationCounts(counts["planned"]), "a zero row count is still one row")
	require.Equal(t, "none", formatMutationCounts(counts["applied"]))

	findings := []reconcile.FindingRecord{{Status: "open"}, {Status: "open"}, {Status: "resolved"}}
	require.Equal(t, "open=2, resolved=1", formatFindingStatusCounts(findingStatusCounts(findings)))
	require.Equal(t, "none", formatFindingStatusCounts(nil))

	plan := []pullProviderPruneLog{
		{Provider: "nmi", Result: reconcile.PruneResult{Subscriptions: 2, SubscriptionsSkipped: 1, Payments: 3, CheckoutSessions: 4}},
		{Provider: "stripe", Result: reconcile.PruneResult{Payments: 1}},
	}
	require.Equal(t, "checkout_sessions would_prune=4; payments would_prune=4; subscriptions would_prune=2 skipped=1", formatPruneCounts(summarizePrune(plan)))
	require.Equal(t, 6, prunePlanTotal(plan), "the typed confirmation counts subscriptions and payments, not cascaded dependents")
	require.False(t, prunePlanApplied(plan))

	subs, pays := []uuid.UUID{uuid.New()}, []uuid.UUID{uuid.New(), uuid.New()}
	records := pruneMutationRecords("nmi", reconcile.PruneResult{SubscriptionIDs: subs, PaymentIDs: pays}, "planned")
	require.Len(t, records, 3)
	for _, rec := range records {
		require.Equal(t, "soft_delete", rec.Operation, "prune never hard-deletes")
		require.Equal(t, "planned", rec.Phase)
	}
}

// A dry-run prune tells the operator the exact confirmation to type; an
// applied one names the run that reverses it.
func TestPullProviderStdoutGuidesPrune(t *testing.T) {
	run := reconcile.RunRecord{ID: uuid.New(), Status: "completed"}
	res := &reconcile.RunResult{}
	var buf bytes.Buffer
	plan := []pullProviderPruneLog{{Provider: "nmi", Binding: reconcile.PSPBinding{AccountID: "100001"}, Result: reconcile.PruneResult{Subscriptions: 2, Payments: 1}}}
	require.NoError(t, renderPullProviderStdout(&buf, "table", "run.log", run, res, nil, plan, nil))
	require.Contains(t, buf.String(), "PLAN ONLY — nothing was written. To apply: re-run with --expect-rows 3")

	buf.Reset()
	destructive := uuid.New()
	applied := []pullProviderPruneLog{{Provider: "nmi", Binding: reconcile.PSPBinding{AccountID: "100001"}, Applied: true, Result: reconcile.PruneResult{RunID: destructive, Subscriptions: 2}}}
	require.NoError(t, renderPullProviderStdout(&buf, "table", "run.log", run, res, nil, applied, &pullProviderConvergeLog{Findings: 1}))
	out := buf.String()
	require.NotContains(t, out, "PLAN ONLY")
	require.Contains(t, out, "--run "+destructive.String())
	require.Contains(t, out, "prune: subscriptions soft_deleted=2")
	require.Contains(t, out, "converge: 1 finding(s)")
}
