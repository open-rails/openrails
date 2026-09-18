package handlers

import (
	"errors"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/pkg/api"
)

// writeRefusal answers a typed service refusal (#983) by its status and code.
// Any other error is an internal failure: a stable message, the cause logged
// against the request id, never raw driver text.
func writeRefusal(r *httprequest.Request, err error, internalMessage string) {
	var refusal *apperr.Error
	if !errors.As(err, &refusal) {
		r.InternalError(internalMessage, err)
		return
	}
	out := api.NewAPIError(refusal.Status, api.ErrorTypeForStatus(refusal.Status), refusal.Code, err.Error())
	if refusal.Param != "" {
		out.WithParam(refusal.Param)
	}
	r.APIError(out)
}
