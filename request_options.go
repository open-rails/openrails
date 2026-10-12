package openrails

import "strings"

// RequestOption configures one operation without changing the shared Client.
type RequestOption func(*requestOptions)

type requestOptions struct {
	idempotencyKey string
}

// WithIdempotencyKey sends key on an operation that reads Idempotency-Key,
// such as CreateOrder or RefundPayment. If params also names a key, they
// must agree. Operations identified by request_id, source/source_id or
// reference use those identities instead; this option does not cache their
// responses or replace their durable identity.
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

// requiredIdempotencyKey resolves the two SDK spellings of an operation key.
func requiredIdempotencyKey(key string, options []RequestOption) (string, error) {
	key = strings.TrimSpace(key)
	if option := idempotencyKey(options); option != "" {
		if key != "" && key != option {
			return "", invalidErr("Idempotency-Key differs between params and request option")
		}
		key = option
	}
	if key == "" {
		return "", invalidErr("Idempotency-Key required")
	}
	return key, nil
}
