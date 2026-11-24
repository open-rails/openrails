package alerting

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/reconcile"
)

// DeliveryKind is the operator alert job.
const DeliveryKind = "openrails.operator_alert"

// DeliveryArgs is one operator alert: its fanout to the configured channels
// (Channel nil), or its delivery to one. Its source is a destructive-action
// hold or a finding's notification episode. It stores channel references,
// never webhook URLs or credentials.
type DeliveryArgs struct {
	MerchantID uuid.UUID     `json:"merchant_id"`
	Hold       *HoldAlert    `json:"hold,omitempty"`
	Finding    *FindingAlert `json:"finding,omitempty"`
	Channel    *ChannelRef   `json:"channel,omitempty"`
}

// HoldAlert is a destructive-action incident.
type HoldAlert struct {
	ID       uuid.UUID `json:"id"`
	Scope    string    `json:"scope"`
	OpenedAt time.Time `json:"opened_at"`
}

// FindingAlert is one notification episode of a finding: the notified_at its
// claim stamped.
type FindingAlert struct {
	ID         uuid.UUID `json:"id"`
	NotifiedAt time.Time `json:"notified_at"`
}

func (DeliveryArgs) Kind() string { return DeliveryKind }

// InsertOpts retries each delivery on its own. Arguments deduplicate, completed
// jobs included, so a delivered alert is never sent again.
func (DeliveryArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "billing", MaxAttempts: 100, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{
		rivertype.JobStateAvailable, rivertype.JobStateCompleted, rivertype.JobStatePending,
		rivertype.JobStateRetryable, rivertype.JobStateRunning, rivertype.JobStateScheduled}}}
}

// Valid reports whether a names its merchant and exactly one source.
func (a DeliveryArgs) Valid() bool {
	switch {
	case a.MerchantID == uuid.Nil || (a.Hold == nil) == (a.Finding == nil):
		return false
	case a.Hold != nil:
		return a.Hold.ID != uuid.Nil
	default:
		return a.Finding.ID != uuid.Nil
	}
}

// queueFindingAlert commits the alert of a finding's notification episode in
// tx, the transaction that claimed it. A low finding stays in the console.
func queueFindingAlert(ctx context.Context, database *db.DB, tx pgx.Tx, merchantID, findingID uuid.UUID, severity reconcile.Severity, notifiedAt time.Time) error {
	if severity == reconcile.SeverityLow {
		return nil
	}
	_, err := database.InsertRiverJobTx(ctx, tx, DeliveryArgs{MerchantID: merchantID, Finding: &FindingAlert{ID: findingID, NotifiedAt: notifiedAt}}, nil)
	if err != nil {
		return fmt.Errorf("queue finding alert: %w", err)
	}
	return nil
}

// QueueRealerts commits the alerts of findings whose notification tx re-armed
// and claimed.
func (s *Service) QueueRealerts(ctx context.Context, tx pgx.Tx, claimed []gen.BillingReconciliationFinding) error {
	for _, f := range claimed {
		if f.NotifiedAt == nil {
			return fmt.Errorf("re-alert of finding %s claimed no episode", f.ID)
		}
		if err := queueFindingAlert(ctx, s.db, tx, f.MerchantID, f.ID, reconcile.Severity(f.Severity), *f.NotifiedAt); err != nil {
			return err
		}
	}
	return nil
}

// Channels snapshots the destinations of a's alert: the alert email, and the
// enabled alert webhooks. A finding's severity selects among them; one whose
// episode ended has none. Call outside a transaction: configuration may use
// Vault.
func (s *Service) Channels(ctx context.Context, a DeliveryArgs) ([]ChannelRef, error) {
	if s == nil || s.store == nil || s.store.config == nil {
		return nil, errors.New("operator alert service is not configured")
	}
	email, hooks := true, true
	if a.Finding != nil {
		row, current, err := s.episode(ctx, *a.Finding)
		if err != nil || !current {
			return nil, err
		}
		sev := reconcile.Severity(row.Severity)
		email, hooks = findingAlertSeverity(sev) == SeverityCritical, sev != reconcile.SeverityLow
	}
	out := []ChannelRef{}
	if email {
		recipient, err := s.alertEmail(ctx)
		if err != nil {
			return nil, err
		}
		if recipient != "" {
			out = append(out, ChannelRef{Type: ChannelEmail})
		}
	}
	if hooks {
		list, err := s.store.listWebhooks(ctx, nil)
		if err != nil {
			return nil, err
		}
		for _, h := range list {
			if h.Enabled {
				id := h.ID
				out = append(out, ChannelRef{Type: ChannelWebhook, WebhookID: &id})
			}
		}
	}
	return out, nil
}

// Deliver attempts a's delivery to its channel. Only success completes its
// job; failures stay retryable. A repeated webhook delivery carries the same
// idempotency key; email is at-least-once across a lost ACK. A finding whose
// episode ended, or a destination removed or disabled, is sent nothing.
func (s *Service) Deliver(ctx context.Context, a DeliveryArgs) error {
	if a.Channel == nil {
		return errors.New("operator alert delivery names no channel")
	}
	var alert Alert
	var key string
	switch {
	case a.Hold != nil:
		h := a.Hold
		alert = Alert{Title: "Destructive actions held for operator review", Severity: SeverityCritical,
			Summary:       fmt.Sprintf("A %s destructive-action ceiling tripped. New actions remain held until an authorized operator clears incident %s.", h.Scope, h.ID),
			DashboardLink: s.opsLink("destructive_hold=" + h.ID.String()), FiredAt: h.OpenedAt}
		key = "destructive-hold:" + h.ID.String()
	case a.Finding != nil:
		row, current, err := s.episode(ctx, *a.Finding)
		if err != nil || !current {
			return err
		}
		alert = s.findingAlert(reconcile.FindingRecordFromRow(row), a.Finding.NotifiedAt)
		key = "finding:" + row.ID.String() + ":" + strconv.FormatInt(a.Finding.NotifiedAt.UnixMicro(), 10)
	default:
		return errors.New("operator alert names no source")
	}
	return s.deliverOnce(ctx, *a.Channel, alert, key)
}

// episode reads the finding f names; current while f is its open notification
// episode.
func (s *Service) episode(ctx context.Context, f FindingAlert) (gen.BillingReconciliationFinding, bool, error) {
	var row gen.BillingReconciliationFinding
	err := s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var err error
		row, err = s.db.Gen(ctx).GetReconciliationFinding(ctx, f.ID)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return row, false, nil
	}
	if err != nil {
		return row, false, err
	}
	current := row.Status == string(reconcile.FindingStatusRequiresReview) && row.NotifiedAt != nil && row.NotifiedAt.Equal(f.NotifiedAt)
	return row, current, nil
}

// alertEmail is the merchant's alert address; empty when it has none.
func (s *Service) alertEmail(ctx context.Context) (string, error) {
	var recipient string
	err := s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		settings, _, err := s.deliverer.cfgStore.Get(ctx)
		recipient = strings.TrimSpace(settings.AlertEmail)
		return err
	})
	return recipient, err
}

// deliverOnce makes one attempt to send alert to channel; key identifies the
// alert to a webhook across retries.
func (s *Service) deliverOnce(ctx context.Context, channel ChannelRef, alert Alert, key string) error {
	switch {
	case channel.Type == ChannelEmail:
		recipient, err := s.alertEmail(ctx)
		if err != nil {
			return err
		}
		if recipient == "" {
			return nil // The operator removed the destination.
		}
		if s.deliverer.email == nil || !s.deliverer.email.IsEnabled() {
			return errors.New("operator alert email sender is not configured")
		}
		html, plain := renderEmail(alert)
		if err := s.deliverer.email.SendEmail(ctx, recipient, alertTitle(alert), html, plain); err != nil {
			return errors.New("operator alert email delivery failed")
		}
		return nil
	case channel.Type == ChannelWebhook && channel.WebhookID != nil:
		hook, err := s.store.getWebhook(ctx, *channel.WebhookID)
		if errors.Is(err, ErrWebhookNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if !hook.Enabled {
			return nil
		}
		body, err := shapeWebhookBody(hook.Format, alert)
		if err != nil {
			return err
		}
		if err = s.outbound.ValidateURL(hook.url); err != nil {
			return errors.New("operator alert destination refused")
		}
		status, err := s.deliverer.post(ctx, hook.url, body, key+":"+hook.ID.String())
		if err != nil {
			return errors.New("operator alert webhook delivery failed")
		}
		if status < 200 || status >= 300 {
			return fmt.Errorf("operator alert webhook returned status %d", status)
		}
		return nil
	default:
		return errors.New("invalid operator alert channel")
	}
}
