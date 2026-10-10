package middleware

// DefaultMaxBodyBytes is the default request body cap (RequestLimitsHTTP).
// Webhook routes get it too, as a backstop; the per-rail caps in
// handlers/webhook.go bind tighter.
const DefaultMaxBodyBytes int64 = 1 << 20
