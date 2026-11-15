// Reconciliation findings produce durable, deduplicated alert deliveries.
package alerting

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/reconcile"
)

var _ reconcile.FindingNotifier = (*Service)(nil)

// NotifyFinding is called by both reconcile engines after persisting a finding.
// Only requires_review findings notify: one the engine can enforce stays
// reconcile_required and self-heals, so notifying on severity alone would page
// for problems already fixed. Low-severity findings stay in the console.
//
// A set NotifiedAt blocks re-firing unless severity strictly escalates; every
// resolution clears it, so a reopened finding notifies again.
func (s *Service) NotifyFinding(ctx context.Context, rec reconcile.FindingRecord) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	if rec.Status != reconcile.FindingStatusRequiresReview || rec.Quiet() {
		return nil
	}
	if rec.NotifiedAt != nil && reconcile.SeverityRank(rec.Severity) >= reconcile.SeverityRank(reconcile.Severity(rec.NotifiedSeverity)) {
		return nil // already notified at this severity or worse; not a genuine escalation
	}
	// The episode claim and its alert's job commit together: concurrent or
	// stale callers claim nothing, and a claimed episode is delivered with
	// retries. The episode is the stored instant, so it is microseconds.
	notifiedAt := s.now().Truncate(time.Microsecond)
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		n, err := s.db.NewWithPgxTx(tx).Gen(ctx).ClaimReconciliationFindingNotification(ctx, gen.ClaimReconciliationFindingNotificationParams{
			MerchantID: mid.UUID(), ID: rec.ID, NotifiedAt: notifiedAt, Severity: string(rec.Severity),
		})
		if err != nil || n == 0 {
			return err
		}
		return s.queueFindingAlert(ctx, tx, mid.UUID(), rec.ID, rec.Severity, notifiedAt)
	})
	if err != nil {
		return fmt.Errorf("claim finding notification: %w", err)
	}
	return nil
}

// findingAlert is rec's alert, fired when its episode was claimed.
func (s *Service) findingAlert(rec reconcile.FindingRecord, firedAt time.Time) Alert {
	return Alert{
		Severity:      findingAlertSeverity(rec.Severity),
		Title:         findingTitle(rec),
		Summary:       findingSummary(rec),
		DashboardLink: s.findingLink(rec.ID),
		FiredAt:       firedAt,
	}
}

// findingAlertSeverity maps a reconcile finding severity onto the coarser
// alerting channel-routing severity (critical/high -> critical fans out to
// email; medium/low -> warning).
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

// findingLink links to the retained console findings view.
func (s *Service) findingLink(id uuid.UUID) string {
	return s.opsLink("finding=" + id.String())
}

func (s *Service) opsLink(query string) string {
	path := "/ops?" + query
	if s.dashboardBaseURL == "" {
		return path
	}
	return s.dashboardBaseURL + path
}
