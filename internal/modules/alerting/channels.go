package alerting

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/shared/httpx"
)

// EmailSender is the config/host seam for outbound alert email. The SendGrid-
// backed subscriptions.EmailService satisfies it; a host can inject its own.
// A nil sender or one that IsEnabled()==false fails soft (the delivery record
// notes the skip and the dev path logs loudly).
type EmailSender interface {
	SendEmail(ctx context.Context, to, subject, htmlContent, plainContent string) error
	IsEnabled() bool
}

// deliverer fans an alert out to a rule's channels and returns per-channel
// delivery results (recorded on the notification, returned by test-fire).
type deliverer struct {
	store       *store
	cfgStore    *merchantconfig.Store
	email       EmailSender
	outbound    httpx.Policy
	http        *http.Client
	backoff     time.Duration
	maxAttempts int
}

func newDeliverer(database *db.DB, st *store, email EmailSender, outbound httpx.Policy, backoff time.Duration) *deliverer {
	if backoff <= 0 {
		backoff = 500 * time.Millisecond
	}
	return &deliverer{
		store:    st,
		cfgStore: merchantconfig.NewStore(database),
		email:    email,
		outbound: outbound,
		// #SEC-21: the delivery client is address-guarded at the DIALER, so a
		// sink whose DNS flips between the 3 retries still cannot reach an
		// internal address, and redirects are re-validated per hop.
		http:        outbound.Client(10 * time.Second),
		backoff:     backoff,
		maxAttempts: 3,
	}
}

// dispatch delivers the alert to every channel on the rule, in order.
func (d *deliverer) dispatch(ctx context.Context, rule Rule, alert Alert) []DeliveryResult {
	return d.dispatchChannels(ctx, rule.Channels, &rule.ID, alert)
}

// dispatchFinding delivers an alert built from a reconciliation finding (#787)
// through an explicit channel set rather than a rule — findings are not
// metric-threshold rules, so there is no Rule row to carry channels; callers
// pass the severity-based default set instead. The stored notification's
// rule_id stays NULL (informational-only column; no rule produced this alert).
func (d *deliverer) dispatchFinding(ctx context.Context, channels []ChannelRef, alert Alert) []DeliveryResult {
	return d.dispatchChannels(ctx, channels, nil, alert)
}

func (d *deliverer) dispatchChannels(ctx context.Context, channels []ChannelRef, ruleID *uuid.UUID, alert Alert) []DeliveryResult {
	results := make([]DeliveryResult, 0, len(channels))
	for _, ch := range channels {
		switch ch.Type {
		case ChannelInApp:
			results = append(results, d.deliverInApp(ctx, ruleID, alert))
		case ChannelEmail:
			results = append(results, d.deliverEmail(ctx, alert))
		case ChannelWebhook:
			results = append(results, d.deliverWebhook(ctx, ch, alert))
		default:
			results = append(results, DeliveryResult{Channel: string(ch.Type), OK: false, Detail: "unknown channel type"})
		}
	}
	return results
}

func (d *deliverer) deliverInApp(ctx context.Context, ruleID *uuid.UUID, alert Alert) DeliveryResult {
	_, err := d.store.createNotification(ctx, Notification{
		Severity: alert.Severity,
		Title:    alertTitle(alert),
		Body:     alert.Summary,
		Link:     alert.DashboardLink,
		RuleID:   ruleID,
		Data:     alert,
	})
	if err != nil {
		return DeliveryResult{Channel: string(ChannelInApp), OK: false, Detail: err.Error()}
	}
	return DeliveryResult{Channel: string(ChannelInApp), OK: true}
}

func (d *deliverer) deliverEmail(ctx context.Context, alert Alert) DeliveryResult {
	to := d.resolveAlertEmail(ctx)
	if to == "" {
		return DeliveryResult{Channel: string(ChannelEmail), OK: false, Detail: "no alert_email configured (fail-soft skip)"}
	}
	if d.email == nil || !d.email.IsEnabled() {
		// Dev fallback: log loudly so the alert is not lost silently.
		log.WithContext(ctx).WithField("alert", alert.Summary).
			Warn("alerting: email channel selected but no email sender is configured; skipping (would send to " + to + ")")
		return DeliveryResult{Channel: string(ChannelEmail), OK: false, Detail: "email sender not configured (logged; fail-soft skip)"}
	}
	subject := alertTitle(alert)
	html, plain := renderEmail(alert)
	if err := d.email.SendEmail(ctx, to, subject, html, plain); err != nil {
		return DeliveryResult{Channel: string(ChannelEmail), OK: false, Detail: err.Error()}
	}
	return DeliveryResult{Channel: string(ChannelEmail), OK: true}
}

func (d *deliverer) deliverWebhook(ctx context.Context, ch ChannelRef, alert Alert) DeliveryResult {
	label := string(ChannelWebhook)
	if ch.WebhookID == nil {
		return DeliveryResult{Channel: label, OK: false, Detail: "webhook channel missing webhook_id"}
	}
	label = "webhook:" + ch.WebhookID.String()
	wh, err := d.store.getWebhook(ctx, *ch.WebhookID)
	if err != nil {
		return DeliveryResult{Channel: label, OK: false, Detail: "webhook not found"}
	}
	if !wh.Enabled {
		return DeliveryResult{Channel: label, OK: false, Detail: "webhook disabled"}
	}
	body, err := shapeWebhookBody(wh.Format, alert)
	if err != nil {
		return DeliveryResult{Channel: label, OK: false, Detail: err.Error()}
	}
	// Re-validate at delivery: a sink stored before the policy tightened, or
	// one whose scheme/host was edited out of band, never gets fetched (#SEC-21).
	if err := d.outbound.ValidateURL(wh.URL); err != nil {
		return DeliveryResult{Channel: label, OK: false, Detail: httpx.FailureDetail(err)}
	}
	attempts, err := d.postWithRetry(ctx, wh.URL, body)
	res := DeliveryResult{Channel: label, Attempts: attempts, OK: err == nil}
	if err != nil {
		// #SEC-21: the raw dial error/status is an internal-network oracle —
		// a tenant-driven port scanner. Log it, return a fixed message.
		log.WithContext(ctx).WithError(err).WithField("webhook_id", ch.WebhookID.String()).
			Warn("alerting: webhook delivery failed")
		res.Detail = httpx.FailureDetail(err)
	}
	return res
}

// postWithRetry POSTs the shaped body with modest exponential backoff. Success
// is any 2xx; the last error is returned after maxAttempts.
func (d *deliverer) postWithRetry(ctx context.Context, url string, body []byte) (int, error) {
	var lastErr error
	for attempt := 1; attempt <= d.maxAttempts; attempt++ {
		if attempt > 1 {
			delay := d.backoff * time.Duration(1<<(attempt-2))
			select {
			case <-ctx.Done():
				return attempt - 1, ctx.Err()
			case <-time.After(delay):
			}
		}
		status, err := d.postOnce(ctx, url, body)
		if err == nil && status >= 200 && status < 300 {
			return attempt, nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("webhook POST returned status %d", status)
		}
	}
	return d.maxAttempts, lastErr
}

func (d *deliverer) postOnce(ctx context.Context, url string, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	return resp.StatusCode, nil
}

func (d *deliverer) resolveAlertEmail(ctx context.Context) string {
	if d.cfgStore == nil {
		return ""
	}
	cfg, _, err := d.cfgStore.Get(ctx)
	if err != nil {
		log.WithContext(ctx).WithError(err).Warn("alerting: could not read merchant config for alert_email")
		return ""
	}
	return strings.TrimSpace(cfg.AlertEmail)
}

// shapeWebhookBody renders the alert per the webhook format: generic=our alert
// JSON, discord={content}, slack={text}. Discord/Slack native channel-webhook
// URLs accept these directly (no bot).
func shapeWebhookBody(format WebhookFormat, alert Alert) ([]byte, error) {
	switch format {
	case FormatDiscord:
		return json.Marshal(map[string]string{"content": chatMessage(alert)})
	case FormatSlack:
		return json.Marshal(map[string]string{"text": chatMessage(alert)})
	case FormatGeneric, "":
		return json.Marshal(alert)
	default:
		return nil, errors.New("unknown webhook format " + string(format))
	}
}

func alertTitle(alert Alert) string {
	prefix := ""
	if alert.Test {
		prefix = "[TEST] "
	}
	return fmt.Sprintf("%s[%s] %s", prefix, strings.ToUpper(string(alert.Severity)), displayName(alert))
}

func displayName(alert Alert) string {
	if strings.TrimSpace(alert.RuleName) != "" {
		return alert.RuleName
	}
	if def, ok := templates[alert.Template]; ok {
		return def.displayName
	}
	return alert.Template
}

func chatMessage(alert Alert) string {
	var b strings.Builder
	b.WriteString(alertTitle(alert))
	b.WriteString("\n")
	b.WriteString(alert.Summary)
	if alert.DashboardLink != "" {
		b.WriteString("\n")
		b.WriteString(alert.DashboardLink)
	}
	return b.String()
}

func renderEmail(alert Alert) (html, plain string) {
	dims := ""
	if len(alert.Dimensions) > 0 {
		parts := make([]string, 0, len(alert.Dimensions))
		for k, v := range alert.Dimensions {
			parts = append(parts, fmt.Sprintf("%s=%s", k, v))
		}
		dims = strings.Join(parts, ", ")
	}
	link := ""
	if alert.DashboardLink != "" {
		link = alert.DashboardLink
	}
	html = fmt.Sprintf(`<h2>%s</h2><p>%s</p>`, alertTitle(alert), alert.Summary)
	if dims != "" {
		html += fmt.Sprintf(`<p><strong>Dimensions:</strong> %s</p>`, dims)
	}
	if link != "" {
		html += fmt.Sprintf(`<p><a href="%s">Open dashboard</a></p>`, link)
	}
	plain = alertTitle(alert) + "\n\n" + alert.Summary
	if dims != "" {
		plain += "\nDimensions: " + dims
	}
	if link != "" {
		plain += "\nDashboard: " + link
	}
	return html, plain
}
