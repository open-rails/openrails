package reconcile

import "context"

// FindingNotifier is the #787 operator-notification seam: both writers of the
// reconciliation_findings ledger (the pull Engine below, and the Convergence
// Engine's DERIVE/LIFE/CON passes) call NotifyFinding once per persisted
// finding row. Findings are event-sourced (one row upserted per pass) rather
// than a metric-threshold rule, so this bridges directly into the #736
// notification store instead of going through the alert_rules/evaluator
// registry. Implementations own the requires_review/severity predicate,
// dedupe (FindingRecord.NotifiedAt/NotifiedSeverity), the low-severity digest,
// and channel selection. nil is a valid no-op — embedded runtimes wire no
// alerting service.
type FindingNotifier interface {
	NotifyFinding(ctx context.Context, rec FindingRecord) error
}
