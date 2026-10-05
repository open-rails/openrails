package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/dashboard"
	"github.com/open-rails/openrails/internal/modules/metrics"
)

// GetMerchantDashboard handles GET /v1/merchant/dashboard: the saved widget
// layout, or the seeded default template when the merchant has none (#741).
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
	uc, _ := r.UserContext()
	d, err := svc.Put(r.Request.Context(), widgets, uc.UserID)
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

// GenerateDashboardWidget handles POST /v1/merchant/dashboard/widgets/generate:
// prompt → VALIDATED {query, title, viz} via the server-side LLM (#741). The
// LLM sees only the metrics schema, never data. The route is registered only
// on deployments with an LLM key; the console keys on /admin/config.json.
// Optional base_query (an existing widget's query, validated like any client
// query) makes the prompt a REFINEMENT of that query instead of a fresh start.
func GenerateDashboardWidget(r *httprequest.Request) {
	svc := r.State.DashboardService
	if !svc.NLConfigured() {
		r.ErrorCode(billing.CodeServiceUnavailable, "widget generation unavailable")
		return
	}
	var body billing.GenerateDashboardWidgetParams
	if !r.BindJSON(&body) {
		return
	}
	if strings.TrimSpace(body.Prompt) == "" {
		r.APIError(api.Coded(billing.CodeInvalidParam, "prompt is required").WithParam("prompt"))
		return
	}
	base := body.BaseQuery
	if base != nil {
		if _, verr := metrics.Validate(base); verr != nil {
			for i := range verr.Errors {
				verr.Errors[i].Param = "base_query." + verr.Errors[i].Param
			}
			dashboardValidationError(r, verr)
			return
		}
	}
	res, err := svc.Generate(r.Request.Context(), strings.TrimSpace(body.Prompt), base)
	if err != nil {
		var invalid *dashboard.GenerateInvalidError
		switch {
		case errors.As(err, &invalid):
			r.APIError(api.NewAPIError(http.StatusUnprocessableEntity, api.ErrorTypeInvalidRequest, "widget_generation_invalid",
				"the model could not produce a valid query for that prompt — try rephrasing").
				WithMetadata(map[string]any{"errors": invalid.Errors}))
		default:
			modelFailure(r, err, false)
		}
		return
	}
	r.JSON(http.StatusOK, res)
}

func dashboardValidationError(r *httprequest.Request, verr *metrics.ValidationError) {
	r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "dashboard_invalid", verr.Error()).
		WithMetadata(map[string]any{"errors": verr.Errors}))
}
