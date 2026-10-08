package reconcile

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// A completed provider response alone cannot authorize absence-based repairs:
// the mirror must have applied it, with no unresolved or policy-withheld work.
func TestPullProofRequiresCompletedMirrorApplication(t *testing.T) {
	for _, test := range []struct {
		name string
		mode Mode
		edit func(*ProviderReport)
		want bool
	}{
		{"applied", ModeEnforce, func(*ProviderReport) {}, true},
		{"advisory", ModeAdvisory, func(*ProviderReport) {}, false},
		{"fetch failed", ModeEnforce, func(r *ProviderReport) { r.Error = "page unavailable" }, false},
		{"partial apply", ModeEnforce, func(r *ProviderReport) { r.ApplyErrors = []string{"payment write failed"} }, false},
		{"idempotent no-op", ModeEnforce, func(r *ProviderReport) { r.ApplySkipped = 1 }, true},
		{"policy held repair", ModeEnforce, func(r *ProviderReport) { r.WithheldChanges = 1 }, false},
		{"conflicting identity", ModeEnforce, func(r *ProviderReport) { r.RequiresReview = 1 }, false},
		{"aborted roster", ModeEnforce, func(r *ProviderReport) { r.Aborted = true }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			report := &ProviderReport{PspID: uuid.NewString(), Coverage: SnapshotCoverage{SubscriptionsExhaustive: true}}
			test.edit(report)
			result := &RunResult{Mode: test.mode, Summary: &RunSummary{Providers: map[string]*ProviderReport{"nmi": report}}}
			proof, ok := result.PullProofs()[ProviderNMI]
			require.Equal(t, test.want, ok)
			if ok {
				require.Equal(t, report.PspID, proof.PspID)
				require.True(t, proof.Coverage.SubscriptionsExhaustive)
			}
		})
	}
}

func TestAppliedEventCoverageSeparatesReceiptsFromHeldLifecycle(t *testing.T) {
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	until := since.Add(24 * time.Hour)
	for _, test := range []struct {
		name string
		edit func(*RunResult, *ProviderReport)
		want bool
	}{
		{"complete", func(*RunResult, *ProviderReport) {}, true},
		{"advisory only", func(r *RunResult, _ *ProviderReport) { r.Mode = ModeAdvisory }, false},
		{"pagination incomplete", func(_ *RunResult, p *ProviderReport) { p.Coverage.TransactionsPaginatedComplete = false }, false},
		{"apply failure", func(_ *RunResult, p *ProviderReport) { p.ApplyErrors = []string{"receipt transaction rolled back"} }, false},
		{"withheld cancellation", func(r *RunResult, _ *ProviderReport) {
			r.Findings = []FindingRecord{{Provider: ProviderNMI, Type: FindingLocalActiveRemoteDead, Status: FindingStatusReconcileRequired}}
		}, true},
		{"unresolved receipt", func(r *RunResult, _ *ProviderReport) {
			r.Findings = []FindingRecord{{Provider: ProviderNMI, Type: FindingChargeMissingLocal, Status: FindingStatusRequiresReview}}
		}, false},
		{"incomplete window", func(_ *RunResult, p *ProviderReport) {
			end := until.Add(-time.Second)
			p.Coverage.TransactionWindowUntil = &end
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := &ProviderReport{Coverage: SnapshotCoverage{TransactionsExhaustive: true, TransactionsPaginatedComplete: true, TransactionWindowSince: &since, TransactionWindowUntil: &until}}
			r := &RunResult{Mode: ModeEnforce, Summary: &RunSummary{Providers: map[string]*ProviderReport{"nmi": p}}}
			test.edit(r, p)
			require.Equal(t, test.want, r.AppliedEventCoverage(ProviderNMI, since, until))
		})
	}
}
