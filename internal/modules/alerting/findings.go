// Reconciliation findings produce durable, deduplicated alert deliveries.
package alerting

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/modules/alerting/alertjob"
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
	return notifyFinding(ctx, s.db, rec, s.now())
}

// notifyFinding claims rec's notification episode at now and queues its alert
// in one transaction.
func notifyFinding(ctx context.Context, database *db.DB, rec reconcile.FindingRecord, now time.Time) error {
	return alertjob.Notify(ctx, database, alertjob.Finding{ID: rec.ID, Status: string(rec.Status), Severity: string(rec.Severity), Quiet: rec.Quiet(),
		NotifiedAt: rec.NotifiedAt, NotifiedSeverity: rec.NotifiedSeverity}, now)
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
