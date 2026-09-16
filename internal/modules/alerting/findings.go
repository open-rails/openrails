// Reconciliation findings produce immediate, deduplicated merchant notifications.
package alerting

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/reconcile"
)

var _ reconcile.FindingNotifier = (*Service)(nil)

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
// Low-severity findings use the deduplicated in-app feed only.
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
	if rec.NotifiedAt != nil && reconcile.SeverityRank(rec.Severity) >= reconcile.SeverityRank(reconcile.Severity(rec.NotifiedSeverity)) {
		return nil // already notified at this severity or worse; not a genuine escalation
	}
	sev := findingAlertSeverity(rec.Severity)
	alert := Alert{
		Severity:      sev,
		Title:         findingTitle(rec),
		Summary:       findingSummary(rec),
		DashboardLink: s.findingLink(rec.ID),
		FiredAt:       s.now(),
	}
	channels := defaultChannels(sev)
	if rec.Severity != reconcile.SeverityLow {
		hooks, err := s.store.listWebhooks(ctx)
		if err != nil {
			return fmt.Errorf("list finding webhook destinations: %w", err)
		}
		for _, hook := range hooks {
			if hook.Enabled {
				id := hook.ID
				channels = append(channels, ChannelRef{Type: ChannelWebhook, WebhookID: &id})
			}
		}
	}
	s.deliverer.dispatchFinding(ctx, channels, alert)
	return s.store.markFindingNotified(ctx, rec.ID, s.now(), string(rec.Severity))
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
