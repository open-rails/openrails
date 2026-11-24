package handlers

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/history"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
)

// ListChanges handles GET /v1/merchant/changes: the merchant's configuration
// history, newest first.
func ListChanges(r *httprequest.Request) {
	var params billing.LogListParams
	if !bindList(r, &params, &params.PageRequest) {
		return
	}
	if r.State.DB == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "configuration history unavailable")
		return
	}
	ctx := r.Request.Context()
	mid, err := merchant.Require(ctx)
	if err != nil {
		writeRefusal(r, err, "configuration history list failed")
		return
	}
	out, err := history.List(ctx, r.State.DB.Gen(ctx), mid.UUID(), params)
	if err != nil {
		writeRefusal(r, err, "configuration history list failed")
		return
	}
	r.SuccessJSON(out)
}
