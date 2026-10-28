// Reconciliation findings produce immediate, deduplicated alert deliveries.
package alerting

import (
	"github.com/open-rails/openrails/internal/merchant"

	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	log "github.com/sirupsen/logrus"

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
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return queryScopeErr
	}

	if rec.Status != reconcile.FindingStatusRequiresReview || rec.Quiet() {
		return nil
	}
	if rec.NotifiedAt != nil && reconcile.SeverityRank(rec.Severity) >= reconcile.SeverityRank(reconcile.Severity(rec.NotifiedSeverity)) {
		return nil // already notified at this severity or worse; not a genuine escalation
	}
	alert, channels, err := s.findingAlert(ctx, rec)
	if err != nil {
		return err
	}
	// The episode claim keeps concurrent or stale callers from sending
	// duplicate hooks; the finding itself is the console's record. External
	// deliveries are best-effort after the claim commits.
	var n int64
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var err error
		n, err = s.db.Gen(ctx).ClaimReconciliationFindingNotification(ctx, gen.ClaimReconciliationFindingNotificationParams{MerchantID: queryMerchant.UUID(),
			ID: rec.ID, NotifiedAt: alert.FiredAt, Severity: string(rec.Severity),
		})
		return err
	})
	if err != nil {
		return fmt.Errorf("claim finding notification: %w", err)
	}
	if n == 0 {
		return nil
	}
	s.deliverFinding(ctx, rec, channels, alert)
	return nil
}

// RealertFinding alerts rec again. The caller claimed its notification
// episode, in the transaction that re-armed it.
func (s *Service) RealertFinding(ctx context.Context, rec reconcile.FindingRecord) error {
	if _, err := merchant.Require(ctx); err != nil {
		return err
	}
	alert, channels, err := s.findingAlert(ctx, rec)
	if err != nil {
		return err
	}
	s.deliverFinding(ctx, rec, channels, alert)
	return nil
}

// findingAlert is rec's alert and the channels it goes to: the severity's
// defaults, and the merchant's enabled webhooks unless it is low.
func (s *Service) findingAlert(ctx context.Context, rec reconcile.FindingRecord) (Alert, []ChannelRef, error) {
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
		hooks, err := s.store.listWebhooks(ctx, nil)
		if err != nil {
			return Alert{}, nil, fmt.Errorf("list finding webhook destinations: %w", err)
		}
		for _, hook := range hooks {
			if hook.Enabled {
				id := hook.ID
				channels = append(channels, ChannelRef{Type: ChannelWebhook, WebhookID: &id})
			}
		}
	}
	return alert, channels, nil
}

func (s *Service) deliverFinding(ctx context.Context, rec reconcile.FindingRecord, channels []ChannelRef, alert Alert) {
	for _, result := range s.deliverer.dispatchFinding(ctx, channels, alert) {
		if !result.OK {
			log.WithContext(ctx).WithFields(log.Fields{"finding_id": rec.ID, "channel": result.Channel}).Warn("finding notification retained in console; external delivery failed")
		}
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
