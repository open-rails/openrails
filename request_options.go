package openrails

import "strings"

// RequestOption configures one operation without changing the shared Client.
type RequestOption func(*requestOptions)

type requestOptions struct {
	idempotencyKey string
}

// WithIdempotencyKey sends key as a programmatic write's Idempotency-Key: a
// retry with it answers the first response. Without it each call sends its
// own key, and the operation's own ids (request, source, operation) keep a
// retry safe.
func WithIdempotencyKey(key string) RequestOption {
	key = strings.TrimSpace(key)
	return func(o *requestOptions) { o.idempotencyKey = key }
}

// idempotencyKey is the key options name, "" for none.
func idempotencyKey(options []RequestOption) string {
	request := requestOptions{}
	for _, option := range options {
		if option != nil {
			option(&request)
		}
	}
	return request.idempotencyKey
}
