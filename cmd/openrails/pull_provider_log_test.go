package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/reconcile"
)

func TestPullProviderLogWritesOneLineMutationEvents(t *testing.T) {
	runID := uuid.New()
	findingID := uuid.New()
	rowID := uuid.New()
	skippedID := uuid.New()
	run := reconcile.RunRecord{
		ID:        runID,
		Mode:      reconcile.ModeEnforce,
		Providers: []string{"nmi"},
		StartedAt: time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC),
		Status:    "completed",
	}
	res := &reconcile.RunResult{
		RunID:  runID,
		Mode:   reconcile.ModeEnforce,
		Status: "completed",
		Findings: []reconcile.FindingRecord{{
			ID:         findingID,
			Provider:   reconcile.ProviderNMI,
			Type:       reconcile.FindingStatusMismatch,
			Status:     reconcile.FindingStatusAutoFixed,
			Severity:   reconcile.SeverityHigh,
			SubjectKey: "sub_remote_1",
			LastSeenAt: time.Date(2026, 6, 24, 12, 5, 0, 0, time.UTC),
		}},
		PlannedChanges: []reconcile.MutationRecord{{
			Phase:        "planned",
			Provider:     reconcile.ProviderNMI,
			FindingID:    findingID,
			FindingType:  reconcile.FindingStatusMismatch,
			SubjectKey:   "sub_remote_1",
			Table:        "subscriptions",
			Operation:    "update",
			RowID:        rowID.String(),
			RowsAffected: 1,
		}},
	}
	applied := []reconcile.MutationRecord{{
		Phase:        "applied",
		Provider:     reconcile.ProviderNMI,
		FindingID:    findingID,
		FindingType:  reconcile.FindingStatusMismatch,
		SubjectKey:   "sub_remote_1",
		Table:        "subscriptions",
		Operation:    "update",
		RowID:        rowID.String(),
		RowsAffected: 1,
		Evidence:     map[string]any{"adopted_status": "cancelled"},
	}}

	path, err := writePullProviderLog(t.TempDir(), run, res, applied, []pullProviderPruneLog{{
		Provider: reconcile.ProviderNMI,
		Binding:  reconcile.ProviderAccountBinding{ID: uuid.New(), AccountID: "acct_1"},
		Result:   reconcile.PruneResult{SkippedSubscriptionIDs: []uuid.UUID{skippedID}},
	}}, &pullProviderConvergeLog{Findings: 2, AutoFixed: 1, ReconcileRequired: 1}, pullProviderMutationFlags{Overwrite: true})
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(path, ".log"))

	body, err := os.ReadFile(path)
	require.NoError(t, err)
	text := string(body)
	require.Contains(t, text, "event=mutation")
	require.Contains(t, text, "phase=planned")
	require.Contains(t, text, "phase=applied")
	require.Contains(t, text, "table=subscriptions")
	require.Contains(t, text, "op=update")
	require.Contains(t, text, "row_id="+rowID.String())
	require.Contains(t, text, "evidence.adopted_status=cancelled")
	require.Contains(t, text, "event=prune_skip")
	require.Contains(t, text, "row_id="+skippedID.String())
	require.Contains(t, text, "event=converge")
	require.Contains(t, text, "event=summary")
}

func TestRenderPullProviderStdoutSummarizesCounts(t *testing.T) {
	runID := uuid.New()
	run := reconcile.RunRecord{ID: runID, Status: "completed"}
	res := &reconcile.RunResult{
		Findings: []reconcile.FindingRecord{
			{Status: reconcile.FindingStatusAutoFixed},
			{Status: reconcile.FindingStatusAdminRequired},
		},
		PlannedChanges: []reconcile.MutationRecord{
			{Phase: "planned", Table: "subscriptions", Operation: "update", RowsAffected: 8},
			{Phase: "planned", Table: "subscriptions", Operation: "insert", RowsAffected: 2},
		},
	}
	applied := []reconcile.MutationRecord{
		{Phase: "applied", Table: "payments", Operation: "delete", RowsAffected: 3},
	}

	var out bytes.Buffer
	err := renderPullProviderStdout(&out, "table", "/tmp/pull.log", run, res, applied, []pullProviderPruneLog{{
		Result: reconcile.PruneResult{Payments: 3, PaymentsSkipped: 2},
	}}, nil)
	require.NoError(t, err)
	text := out.String()
	require.Contains(t, text, "pull-provider run "+runID.String()+" status=completed")
	require.Contains(t, text, "auto_fixed=1")
	require.Contains(t, text, "requires_review=1")
	require.Contains(t, text, "subscriptions inserted=2 updated=8")
	require.Contains(t, text, "payments deleted=3")
	require.Contains(t, text, "prune: payments deleted=3 skipped=2")
	require.Contains(t, text, "log file: /tmp/pull.log")
}
