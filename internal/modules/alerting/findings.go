// Reconciliation findings produce immediate, deduplicated merchant notifications.
package alerting

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	log "github.com/sirupsen/logrus"

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
	// The durable console notification and episode claim commit together. A
	// failed insert rolls the claim back; concurrent/stale callers cannot send
	// duplicate hooks. External deliveries are best-effort after this commit.
	claimed := false
	if err := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		st := newStore(s.db.NewWithPgxTx(tx))
		n, err := st.db.Gen(ctx).ClaimReconciliationFindingNotification(ctx, gen.ClaimReconciliationFindingNotificationParams{
			ID: rec.ID, NotifiedAt: alert.FiredAt, Severity: string(rec.Severity),
		})
		if err != nil || n == 0 {
			return err
		}
		_, err = st.createNotification(ctx, Notification{
			Severity: alert.Severity, Title: alertTitle(alert), Body: alert.Summary,
			Link: alert.DashboardLink, Data: alert,
		})
		claimed = err == nil
		return err
	}); err != nil {
		return fmt.Errorf("persist finding notification: %w", err)
	}
	if !claimed {
		return nil
	}
	for _, result := range s.deliverer.dispatchFinding(ctx, channels, alert) {
		if !result.OK {
			log.WithContext(ctx).WithFields(log.Fields{"finding_id": rec.ID, "channel": result.Channel}).Warn("finding notification retained in console; external delivery failed")
		}
	}
	return nil
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
