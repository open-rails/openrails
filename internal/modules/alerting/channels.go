package alerting

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/shared/httpx"
)

// deliverer sends one alert attempt to a destination.
type deliverer struct {
	cfgStore *merchantconfig.Store
	email    *MailSender
	http     *http.Client
}

func newDeliverer(database *db.DB, email *MailSender, outbound httpx.Policy) *deliverer {
	return &deliverer{
		cfgStore: merchantconfig.NewStore(database),
		email:    email,
		// #SEC-21: the delivery client is address-guarded at the DIALER, so a
		// sink whose DNS flips between retries still cannot reach an internal
		// address, and redirects are re-validated per hop.
		http: outbound.Client(10 * time.Second),
	}
}

// post POSTs body once with its idempotency key and returns the status.
func (d *deliverer) post(ctx context.Context, url string, body []byte, key string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
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

// shapeWebhookBody renders the alert per the webhook format: generic=our alert
// JSON, discord={content}, slack={text}. Discord/Slack native channel-webhook
// URLs accept these directly (no bot). Instants are UTC on the wire.
func shapeWebhookBody(format WebhookFormat, alert Alert) ([]byte, error) {
	alert.FiredAt = alert.FiredAt.UTC()
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

func alertTitle(alert Alert) string {
	return fmt.Sprintf("[%s] %s", strings.ToUpper(string(alert.Severity)), alert.Title)
}

func renderEmail(alert Alert) (htmlBody, plain string) {
	htmlBody = fmt.Sprintf(`<h2>%s</h2><p>%s</p>`, html.EscapeString(alertTitle(alert)), html.EscapeString(alert.Summary))
	plain = alertTitle(alert) + "\n\n" + alert.Summary
	if alert.DashboardLink != "" {
		htmlBody += fmt.Sprintf(`<p><a href="%s">Open dashboard</a></p>`, html.EscapeString(alert.DashboardLink))
		plain += "\nDashboard: " + alert.DashboardLink
	}
	return htmlBody, plain
}
