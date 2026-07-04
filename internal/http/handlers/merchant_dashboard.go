package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/dashboard"
	"github.com/open-rails/openrails/internal/modules/metrics"
	"github.com/open-rails/openrails/pkg/api"
)

// GetMerchantDashboard handles GET /v1/merchant/dashboard: the saved widget
// layout, or the seeded default template when the merchant has none (#741).
func GetMerchantDashboard(r *httprequest.Request) {
	svc := r.State.DashboardService
	if svc == nil {
		r.ErrorJSON(http.StatusServiceUnavailable, "dashboard service not configured")
		return
	}
	d, err := svc.Get(r.Request.Context())
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to load dashboard")
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
		r.ErrorJSON(http.StatusServiceUnavailable, "dashboard service not configured")
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
		r.ErrorJSON(http.StatusInternalServerError, "failed to save dashboard")
		return
	}
	r.JSON(http.StatusOK, d)
}

// GenerateDashboardWidget handles POST /v1/merchant/dashboard/widgets/generate:
// prompt → VALIDATED {query, title, viz} via the server-side LLM (#741). The
// LLM sees only the metrics schema, never data; unconfigured deployments
// answer 501 (fail-closed — the console hides the NL box too).
func GenerateDashboardWidget(r *httprequest.Request) {
	svc := r.State.DashboardService
	if svc == nil {
		r.ErrorJSON(http.StatusServiceUnavailable, "dashboard service not configured")
		return
	}
	if !svc.NLConfigured() {
		r.ErrorJSON(http.StatusNotImplemented,
			"natural-language widget generation is not configured on this deployment: set llm.api_key (env LLM_API_KEY); the manual widget builder works without it")
		return
	}
	var body struct {
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Request.Body).Decode(&body); err != nil || strings.TrimSpace(body.Prompt) == "" {
		r.ErrorJSON(http.StatusBadRequest, `body must be {"prompt":"<what the widget should show>"}`)
		return
	}
	res, err := svc.Generate(r.Request.Context(), strings.TrimSpace(body.Prompt))
	if err != nil {
		var invalid *dashboard.GenerateInvalidError
		switch {
		case errors.As(err, &invalid):
			r.APIError(api.NewAPIError(http.StatusUnprocessableEntity, api.ErrorTypeInvalidRequest, "widget_generation_invalid",
				"the model could not produce a valid query for that prompt — try rephrasing").
				WithMetadata(map[string]any{"errors": invalid.Errors}))
		default:
			r.ErrorJSON(http.StatusBadGateway, "widget generation failed: the LLM request did not complete")
		}
		return
	}
	r.JSON(http.StatusOK, res)
}

func dashboardValidationError(r *httprequest.Request, verr *metrics.ValidationError) {
	r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "dashboard_invalid", verr.Error()).
		WithMetadata(map[string]any{"errors": verr.Errors}))
}
