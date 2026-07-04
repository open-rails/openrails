package handlers

import (
	"bytes"
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
// answer 501 (fail-closed — the console shows a pointed empty-state, #755).
// Optional base_query (an existing widget's query, validated like any client
// query) makes the prompt a REFINEMENT of that query instead of a fresh start.
func GenerateDashboardWidget(r *httprequest.Request) {
	svc := r.State.DashboardService
	if svc == nil {
		r.ErrorJSON(http.StatusServiceUnavailable, "dashboard service not configured")
		return
	}
	if !svc.NLConfigured() {
		r.ErrorJSON(http.StatusNotImplemented,
			"natural-language widget generation is not configured on this deployment: set llm.api_key (env LLM_API_KEY) — see docs/admin-console.md")
		return
	}
	var body struct {
		Prompt    string          `json:"prompt"`
		BaseQuery json.RawMessage `json:"base_query"`
	}
	if err := json.NewDecoder(r.Request.Body).Decode(&body); err != nil || strings.TrimSpace(body.Prompt) == "" {
		r.ErrorJSON(http.StatusBadRequest, `body must be {"prompt":"<what the widget should show>","base_query":<optional existing query to refine>}`)
		return
	}
	var base *metrics.Query
	if len(body.BaseQuery) > 0 && string(body.BaseQuery) != "null" {
		q, verr := metrics.DecodeQuery(bytes.NewReader(body.BaseQuery))
		if verr == nil {
			_, verr = metrics.Validate(q)
		}
		if verr != nil {
			for i := range verr.Errors {
				verr.Errors[i].Param = "base_query." + verr.Errors[i].Param
			}
			dashboardValidationError(r, verr)
			return
		}
		base = q
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
