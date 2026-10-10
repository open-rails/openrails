package handlers

import (
	"net/http"

	"github.com/open-rails/openrails/billing"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/internal/service"
)

// PreviewPSPRouting handles POST /v1/admin/psps/routing-preview: which PSP a
// checkout for this price would use, and why every other PSP was passed over,
// without creating a session.
func PreviewPSPRouting(r *httprequest.Request) {
	var req billing.PreviewPSPRoutingParams
	if !r.BindJSON(&req) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "billing service unavailable")
		return
	}
	out, err := svc.PreviewPSPRouting(r.Request.Context(), req)
	if err != nil {
		writeRefusal(r, err, "PSP routing preview failed")
		return
	}
	r.JSON(http.StatusOK, out)
}
