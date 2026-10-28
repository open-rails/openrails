package handlers

import (
	"github.com/open-rails/openrails/billing"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/parking"
)

// ParkedSubscriptionQuery filters ListParkedSubscriptions. PSP is a PSP's key
// or id; released lists closed parks instead of open ones.
type ParkedSubscriptionQuery struct {
	PSP      string             `form:"psp"`
	Reason   billing.ParkReason `form:"reason"`
	Released bool               `form:"released"`
}

// ListParkedSubscriptions handles GET /v1/merchant/parked-subscriptions: the
// live provider subscriptions OpenRails could not land, newest first.
func ListParkedSubscriptions(r *httprequest.Request) {
	var query ParkedSubscriptionQuery
	if !r.BindQuery(&query) {
		return
	}
	page, ok := r.Page()
	if !ok {
		return
	}
	if r.State.DB == nil || r.State.Merchants == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "parked subscriptions unavailable")
		return
	}
	out, err := parking.List(r.Request.Context(), r.State.DB, r.State.Merchants, billing.ParkedSubscriptionListParams{PageRequest: page, PSP: query.PSP, Reason: query.Reason, Released: query.Released})
	if err != nil {
		writeRefusal(r, err, "parked subscription list failed")
		return
	}
	r.SuccessJSON(out)
}
