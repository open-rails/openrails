package middleware

// DefaultMaxBodyBytes is the default request body-size cap (BodyLimitHTTP).
// Webhook routes are NOT exempted: they get this global cap as a backstop (a
// signature-verified handler still reads the raw body itself, but the cap
// prevents an unbounded read — OR2-DOS-1; the per-rail caps in
// internal/http/handlers/webhook.go bind tighter).
const DefaultMaxBodyBytes int64 = 1 << 20
