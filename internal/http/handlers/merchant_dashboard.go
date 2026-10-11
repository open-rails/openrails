package handlers

import (
	"errors"
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/dashboard"
	"github.com/open-rails/openrails/internal/modules/metrics"
)

// GetMerchantDashboard handles GET /v1/metrics/dashboard: the saved widget
// layout, or the seeded default template when the merchant has none.
func GetMerchantDashboard(r *httprequest.Request) {
	svc := r.State.DashboardService
	if svc == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "dashboard service not configured")
		return
	}
	d, err := svc.Get(r.Request.Context())
	if err != nil {
		r.InternalError("load dashboard failed", err)
		return
	}
	r.JSON(http.StatusOK, d)
}

// PutMerchantDashboard handles PUT /v1/merchant/dashboard: full-replace of the
// widget layout. Every widget query passes the metrics compiler before
// persisting; errors return widget-indexed and ALL AT ONCE.
func PutMerchantDashboard(r *httprequest.Request) {
	svc := r.State.DashboardService
	if svc == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "dashboard service not configured")
		return
	}
	widgets, verr := dashboard.DecodePut(r.Request.Body)
	if verr != nil {
		dashboardValidationError(r, verr)
		return
	}
	staff, ok := r.Staff()
	if !ok {
		r.ErrorCode(billing.CodeAuthenticationRequired, "")
		return
	}
	d, err := svc.Put(r.Request.Context(), widgets, staff.Subject)
	if err != nil {
		var ve *metrics.ValidationError
		if errors.As(err, &ve) {
			dashboardValidationError(r, ve)
			return
		}
		r.InternalError("save dashboard failed", err)
		return
	}
	r.JSON(http.StatusOK, d)
}

func dashboardValidationError(r *httprequest.Request, verr *metrics.ValidationError) {
	r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "dashboard_invalid", verr.Error()).
		WithMetadata(map[string]any{"errors": verr.Errors}))
}
