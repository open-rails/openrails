package guide

import (
	"errors"

	"github.com/open-rails/openrails/billing"
)

// Classify says what a caller does with a Client error.
func Classify(err error) string {
	// guide:errors
	var status *billing.StatusError
	switch {
	case err == nil:
		return "done"
	case errors.Is(err, billing.ErrPaymentRefused): // 402: nothing charged; another card may pay the same order
		return "declined"
	case errors.Is(err, billing.ErrIdempotencyKeyReused): // the key was sent with other terms: a bug
		return "bug"
	case errors.As(err, &status) && status.Status < 500: // a refusal: act on status.Code, never Message
		return status.Code
	default: // no answer, or a 5xx: it may have committed; retry with the same key and terms
		return "retry"
	}
	// guide:end
}
