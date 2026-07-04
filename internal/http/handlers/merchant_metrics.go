package handlers

import (
	"net/http"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/metrics"
	"github.com/open-rails/openrails/pkg/api"
)

// MerchantMetricsSchema handles GET /v1/merchant/metrics/schema: the registry
// dump (measures + formulas + dims + DERIVED formulas + caveats + examples) —
// the client/LLM context document.
func MerchantMetricsSchema(r *httprequest.Request) {
	r.JSON(http.StatusOK, metrics.Schema())
}

// MerchantMetricsQuery handles POST /v1/merchant/metrics/query: the composable
// analytics endpoint (#733). Validation errors come back ALL AT ONCE with
// corrective context (did-you-mean + valid lists).
func MerchantMetricsQuery(r *httprequest.Request) {
	svc := r.State.MetricsService
	if svc == nil {
		r.ErrorJSON(http.StatusServiceUnavailable, "metrics service not configured")
		return
	}
	q, verr := metrics.DecodeQuery(r.Request.Body)
	if verr == nil {
		var plan *metrics.Plan
		plan, verr = metrics.Validate(q)
		if verr == nil {
			res, err := svc.Execute(r.Request.Context(), plan)
			if err != nil {
				r.ErrorJSON(http.StatusInternalServerError, "metrics query failed")
				return
			}
			r.JSON(http.StatusOK, res)
			return
		}
	}
	r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "metrics_query_invalid", verr.Error()).
		WithMetadata(map[string]any{"errors": verr.Errors}))
}
