package alerting

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Finding text is attacker-influenced (provider data); it must never become markup.
func TestEmailEscapesFindingTextAndLink(t *testing.T) {
	alert := Alert{Title: `<img src=x onerror="bad()">`, Summary: `<script>bad()</script> & details`, DashboardLink: `https://example.com/" onclick="bad()`, Severity: SeverityWarning}
	html, plain := renderEmail(alert)
	require.NotContains(t, html, "<img")
	require.NotContains(t, html, "<script>")
	require.NotContains(t, html, `href="https://example.com/" onclick=`)
	require.Contains(t, html, "&lt;script&gt;")
	require.Contains(t, html, "&#34; onclick=&#34;")
	require.Contains(t, plain, alert.Summary)
}

// Discord and Slack channel webhooks accept these bodies natively, with no bot.
func TestWebhookBodyShapes(t *testing.T) {
	alert := Alert{Title: "Webhooks silent", Severity: SeverityCritical, Summary: "No events for 2h", DashboardLink: "https://dash/x"}
	for format, want := range map[WebhookFormat]string{
		FormatDiscord: `{"content":"[CRITICAL] Webhooks silent\nNo events for 2h\nhttps://dash/x"}`,
		FormatSlack:   `{"text":"[CRITICAL] Webhooks silent\nNo events for 2h\nhttps://dash/x"}`,
		FormatGeneric: `{"title":"Webhooks silent","severity":"critical","summary":"No events for 2h","dashboard_link":"https://dash/x","fired_at":"0001-01-01T00:00:00Z"}`,
		"":            `{"title":"Webhooks silent","severity":"critical","summary":"No events for 2h","dashboard_link":"https://dash/x","fired_at":"0001-01-01T00:00:00Z"}`,
	} {
		body, err := shapeWebhookBody(format, alert)
		require.NoError(t, err, format)
		require.JSONEq(t, want, string(body), format)
	}
	_, err := shapeWebhookBody("teams", alert)
	require.ErrorContains(t, err, "unknown webhook format")
}
