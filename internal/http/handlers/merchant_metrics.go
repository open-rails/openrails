package handlers

import (
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/metrics"
)

// MerchantMetricsSchema handles GET /v1/metrics/schema: the registry
// dump (measures + formulas + dims + caveats + examples) —
// the client/LLM context document.
func MerchantMetricsSchema(r *httprequest.Request) {
	r.JSON(http.StatusOK, metrics.Schema())
}

// MerchantMetricsQuery handles POST /v1/metrics/query. Validation errors
// come back all at once with corrective context (did-you-mean + valid lists).
// A relative range ends today on the deployment's clock.
func MerchantMetricsQuery(r *httprequest.Request) {
	svc := r.State.MetricsService
	if svc == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "metrics service not configured")
		return
	}
	q, verr := metrics.DecodeQuery(r.Request.Body)
	if verr == nil {
		var plan *metrics.Plan
		plan, verr = metrics.ValidateAt(q, r.Clock.Now().UTC())
		if verr == nil {
			res, err := svc.Execute(r.Request.Context(), plan)
			if err != nil {
				r.InternalError("metrics query failed", err)
				return
			}
			r.JSON(http.StatusOK, res)
			return
		}
	}
	r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "metrics_query_invalid", verr.Error()).
		WithMetadata(map[string]any{"errors": verr.Errors}))
}
