// #787: bridges internal/reconcile findings into this module's merchant
// notification store + channel fan-out. Findings are event-sourced (one row
// upserted per pull/converge pass) rather than a metric-threshold rule, so
// this is a SEPARATE notification source — it reuses the store/deliverer, not
// the alert_rules/template/evaluator registry.
package alerting

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/reconcile"
)

var _ reconcile.FindingNotifier = (*Service)(nil)

// FindingDigestCadence is the minimum spacing between low-severity findings
// digests. Tighter than the #736 monthly DigestCadence (payment-method
// expiry): findings need faster surfacing, just not one push per finding.
const FindingDigestCadence = 24 * time.Hour

// NotifyFinding is the #787 emit seam both reconcile engines (the pull Engine
// and the Convergence Engine) call after persisting a finding.
//
// Predicate: only status == requires_review findings fire — that status IS
// the finding model's own "needs a human decision" signal (see
// internal/reconcile/findings.go: chargebacks, duplicate subscriptions, and
// ambiguous PS-1 matches are always requires_review because the fix is remote
// or judgment-dependent). A high/critical finding the engine can enforce
// automatically stays reconcile_required and self-heals without ever
// reaching here — notifying on severity alone would page operators for
// problems the system already fixed.
//
// Severity SeverityLow requires_review findings are batched into the digest
// (DigestFindings) instead of firing individually — noise control.
//
// Dedupe: NotifiedAt/NotifiedSeverity on the finding row are the linkage. A
// nil NotifiedAt means this open episode hasn't notified yet. A non-nil one
// blocks re-firing UNLESS severity has genuinely escalated (a strictly lower
// SeverityRank) since the last notify — a re-observation at the same or a
// lower severity is silent. Every resolution path clears the linkage (see
// reconciliation.sql), so a finding that reopens later notifies again.
func (s *Service) NotifyFinding(ctx context.Context, rec reconcile.FindingRecord) error {
	if rec.Status != reconcile.FindingStatusRequiresReview {
		return nil
	}
	if rec.Severity == reconcile.SeverityLow {
		return nil // batched by DigestFindings, not fired here
	}
	if rec.NotifiedAt != nil && reconcile.SeverityRank(rec.Severity) >= reconcile.SeverityRank(reconcile.Severity(rec.NotifiedSeverity)) {
		return nil // already notified at this severity or worse; not a genuine escalation
	}
	sev := findingAlertSeverity(rec.Severity)
	alert := Alert{
		Severity:      sev,
		RuleName:      findingTitle(rec),
		Summary:       findingSummary(rec),
		DashboardLink: s.findingLink(rec.ID),
		FiredAt:       s.now(),
	}
	s.deliverer.dispatchFinding(ctx, defaultChannels(sev), alert)
	return s.store.markFindingNotified(ctx, rec.ID, s.now(), string(rec.Severity))
}

// FindingsDigestStats summarizes one merchant's digest pass.
type FindingsDigestStats struct {
	Digested int64
}

// ArmedFindingsDigestMerchantIDs returns merchants with at least one
// undigested low-severity requires_review finding — the digest sweep's work
// set (indexed; scales with backlog, never the merchant directory, #719).
func (s *Service) ArmedFindingsDigestMerchantIDs(ctx context.Context) ([]uuid.UUID, error) {
	return s.store.listArmedFindingsDigestMerchants(ctx)
}

// DigestFindings runs the merchant's low-severity findings digest if due
// (FindingDigestCadence since the last one) and there is something to report.
// Cadence-gated the same shape as the #736 payment_methods_expiring digest — a
// stored watermark plus a duration check — reused as a PATTERN, not through
// the rule/template registry (findings are not a metric-threshold rule).
func (s *Service) DigestFindings(ctx context.Context) (FindingsDigestStats, error) {
	var stats FindingsDigestStats
	now := s.now()
	last, err := s.store.getFindingDigestWatermark(ctx)
	if err != nil {
		return stats, fmt.Errorf("alerting: findings digest watermark: %w", err)
	}
	if last != nil && now.Sub(*last) < FindingDigestCadence {
		return stats, nil
	}
	n, err := s.store.countLowSeverityFindingsPendingDigest(ctx)
	if err != nil {
		return stats, fmt.Errorf("alerting: count pending findings digest: %w", err)
	}
	if n == 0 {
		// Nothing to report: do not advance the watermark, so a quiet merchant
		// digests immediately once something low-severity actually opens
		// (mirrors the payment_methods_expiring digest's cadence gate).
		return stats, nil
	}
	alert := Alert{
		Severity:      SeverityWarning,
		RuleName:      "Reconciliation findings digest",
		Summary:       fmt.Sprintf("%d low-severity reconciliation finding(s) pending review", n),
		DashboardLink: s.findingsQueueLink(),
		FiredAt:       now,
	}
	s.deliverer.dispatchFinding(ctx, defaultChannels(SeverityWarning), alert)
	if _, err := s.store.markLowSeverityFindingsDigested(ctx, now); err != nil {
		return stats, fmt.Errorf("alerting: mark findings digested: %w", err)
	}
	if err := s.store.touchFindingDigestWatermark(ctx, now); err != nil {
		return stats, fmt.Errorf("alerting: touch findings digest watermark: %w", err)
	}
	stats.Digested = n
	return stats, nil
}

// findingAlertSeverity maps a reconcile finding severity onto the coarser
// alerting channel-routing severity (critical/high -> critical fans out to
// email; medium -> warning stays in_app-only). Low never reaches here (it
// digests instead).
func findingAlertSeverity(sev reconcile.Severity) Severity {
	if sev == reconcile.SeverityCritical || sev == reconcile.SeverityHigh {
		return SeverityCritical
	}
	return SeverityWarning
}

func findingTitle(rec reconcile.FindingRecord) string {
	return fmt.Sprintf("Reconciliation review needed: %s", rec.Type)
}

func findingSummary(rec reconcile.FindingRecord) string {
	msg := fmt.Sprintf("%s finding on %s requires review", rec.Type, rec.SubjectKey)
	if rec.RecommendedAction != "" {
		msg += ": " + rec.RecommendedAction
	}
	return msg
}

// findingLink / findingsQueueLink are deep links into the console Ops tab,
// same relative-path + optional base-URL-prefix shape as dashboardLink.
func (s *Service) findingLink(id uuid.UUID) string {
	return s.opsLink("finding=" + id.String())
}

func (s *Service) findingsQueueLink() string {
	return s.opsLink("severity=low")
}

func (s *Service) opsLink(query string) string {
	path := "/ops?" + query
	if s.dashboardBaseURL == "" {
		return path
	}
	return s.dashboardBaseURL + path
}
