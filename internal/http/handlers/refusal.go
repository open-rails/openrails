package handlers

import (
	"errors"
	"strconv"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/destructive"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// writeRefusal answers a typed service refusal (#983) by its status and code.
// Any other error is an internal failure: a stable message, the cause logged
// against the request id, never raw driver text.
func writeRefusal(r *httprequest.Request, err error, internalMessage string) {
	setRetryAfter(r, err)

	if out := refusalOf(err); out != nil {
		r.APIError(out)
		return
	}
	r.InternalError(internalMessage, err)
}

// setRetryAfter names when a refused destructive command may be tried again:
// a customer action limit's rolling window, or a hold's next look.
func setRetryAfter(r *httprequest.Request, err error) {
	var limited *destructive.UserActionRateError
	switch {
	case errors.As(err, &limited):
		r.SetHeader("Retry-After", strconv.FormatInt(limited.RetryAfterSeconds, 10))
	case errors.Is(err, destructive.ErrHeld):
		r.SetHeader("Retry-After", strconv.FormatInt(int64(destructive.HeldRetryAfter/time.Second), 10))
	}
}

// refusalOf is the answer to a typed refusal, nil for any other error. A
// write posture refusal is retryable, never an internal failure.
func refusalOf(err error) *api.APIError {
	var limited *destructive.UserActionRateError
	if errors.As(err, &limited) {
		return api.Coded(billing.CodeRateLimitExceeded, limited.Error()).WithMetadata(map[string]any{"retry_after_seconds": limited.RetryAfterSeconds})
	}

	if errors.Is(err, destructive.ErrHeld) {
		return api.Coded(billing.CodeRateLimitExceeded, "destructive actions are held until an authorized operator clears the incident")
	}
	if errors.Is(err, destructive.ErrConflict) {
		return api.Coded(billing.CodeIdempotencyKeyReused, "the operation identity already names another destructive action")
	}

	// A refusal a billing type answered, as its Validate does.
	var status *billing.StatusError
	if errors.As(err, &status) && status.Status < 500 {
		out := api.NewAPIError(status.Status, status.Type, status.Code, status.Message)
		if status.Param != nil {
			out.WithParam(*status.Param)
		}
		return out
	}

	// A coded refusal a service already answered in the wire's terms.
	var coded *api.APIError
	if errors.As(err, &coded) && coded.HTTPStatus < 500 {
		return coded
	}
	var refusal *apperr.Error
	if !errors.As(err, &refusal) {
		return nil
	}
	out := api.NewAPIError(refusal.Status, api.ErrorTypeForStatus(refusal.Status), refusal.Code, err.Error())
	if refusal.Status >= 500 {
		out.Message = refusal.Error()
		out.WithCause(err)
	}
	if refusal.Param != "" {
		out.WithParam(refusal.Param)
	}
	if len(refusal.Metadata) > 0 {
		out.Metadata = refusal.Metadata
	}
	return out
}
