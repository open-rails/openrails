package middleware

import "strings"

// DefaultMaxBodyBytes is the default request body-size cap (BodyLimitHTTP).
const DefaultMaxBodyBytes int64 = 1 << 20

// isWebhookPath reports whether path targets a webhook endpoint, normalizing the
// optional embedded "/billing" mount prefix. Webhook bodies are signature-verified
// and must be read raw, so they are exempt from the body-size limit.
func isWebhookPath(path string) bool {
	path = strings.ToLower(path)
	if strings.HasPrefix(path, "/billing") {
		path = strings.TrimPrefix(path, "/billing")
		if path == "" {
			path = "/"
		}
	}
	return strings.HasPrefix(path, "/v1/webhooks") ||
		(strings.HasPrefix(path, "/v1/merchants/") && strings.Contains(path, "/webhooks/"))
}
