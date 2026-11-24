package handlers

import (
	"errors"
	"strconv"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/logx"
)

// StaffCan asks whether the caller would pass the guard of another staff
// route (its key), on the merchant the route already authorized, for items
// objects: its permission, its person rule and its operation limit, spent
// once per object. nil is yes. A handler that runs another route's effect
// itself takes one.
type StaffCan func(r *httprequest.Request, routeKey string, items int64) error

// staffCanAll asks can for each route key; the first refusal is the answer.
func staffCanAll(r *httprequest.Request, can StaffCan, keys []string, items int64) error {
	for _, key := range keys {
		if err := can(r, key, items); err != nil {
			return err
		}
	}
	return nil
}

// refuseStaffCan answers a StaffCan refusal: a gate refusal by its code, an
// operation limit with Retry-After, anything else as authorization being
// unavailable.
func refuseStaffCan(r *httprequest.Request, err error) {
	var limited *middleware.AdminRateLimitError
	var gate billingauth.GateError
	switch {
	case errors.As(err, &limited):
		r.SetHeader("Retry-After", strconv.FormatInt(limited.RetryAfterSeconds(), 10))
		r.APIError(limited.APIError())
	case errors.As(err, &gate) && gate.Code != "":
		r.APIError(billingauth.RefusalError(gate))
	default:
		r.Log.WarnContext(r.Request.Context(), "a staff action could not ask for the guard of the route whose effect it runs", logx.Err(err))
		r.ErrorCode(billing.CodeAuthorizationUnavailable, "")
	}
}
