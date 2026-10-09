package handlers

import (
	"errors"

	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// writeRefusal answers a typed service refusal (#983) by its status and code.
// Any other error is an internal failure: a stable message, the cause logged
// against the request id, never raw driver text.
func writeRefusal(r *httprequest.Request, err error, internalMessage string) {
	if out := refusalOf(err); out != nil {
		r.APIError(out)
		return
	}
	r.InternalError(internalMessage, err)
}

// refusalOf is the answer to a typed refusal, nil for any other error.
func refusalOf(err error) *api.APIError {
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
	return out
}
