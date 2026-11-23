package handlers

import (
	"github.com/open-rails/openrails/billing"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/parking"
)

// ListParkedSubscriptions handles GET /v1/merchant/parked-subscriptions: the
// live provider subscriptions OpenRails could not land, newest first.
func ListParkedSubscriptions(r *httprequest.Request) {
	var params billing.ParkedSubscriptionListParams
	if !bindList(r, &params, &params.PageRequest) {
		return
	}
	if r.State.DB == nil || r.State.Merchants == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "parked subscriptions unavailable")
		return
	}
	out, err := parking.List(r.Request.Context(), r.State.DB, r.State.Merchants, params)
	if err != nil {
		writeRefusal(r, err, "parked subscription list failed")
		return
	}
	r.SuccessJSON(out)
}
