package alerting

import (
	"github.com/stretchr/testify/require"
	"testing"
)

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
