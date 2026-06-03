package middleware

import "strings"

// DefaultMaxBodyBytes is the default request body-size cap shared by the gin
// standalone middleware (ginmw.BodyLimit) and the gin-free embedded assembler
// (BodyLimitHTTP).
const DefaultMaxBodyBytes int64 = 1 << 20

// isWebhookPath reports whether path targets a webhook endpoint, normalizing the
// optional embedded "/billing" mount prefix. Webhook bodies are signature-verified
// and must be read raw, so they are exempt from the body-size limit. This is the
// gin-free analogue of the matcher in the standalone gin middleware (ginmw).
func isWebhookPath(path string) bool {
	path = strings.ToLower(path)
	if strings.HasPrefix(path, "/billing") {
		path = strings.TrimPrefix(path, "/billing")
		if path == "" {
			path = "/"
		}
	}
	return strings.HasPrefix(path, "/v1/webhooks")
}

func isDebugNMITokenizationPath(path string) bool {
	return strings.EqualFold(path, "/debug/nmi/tokenization")
}
