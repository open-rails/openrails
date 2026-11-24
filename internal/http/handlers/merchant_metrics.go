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
// Days are the merchant's accounting timezone's; a relative range ends today
// there, on the deployment's clock.
func MerchantMetricsQuery(r *httprequest.Request) {
	svc := r.State.MetricsService
	if svc == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "metrics service not configured")
		return
	}
	q, verr := metrics.DecodeQuery(r.Request.Body)
	if verr == nil {
		zone, err := svc.Zone(r.Request.Context())
		if err != nil {
			r.InternalError("metrics query failed", err)
			return
		}
		var plan *metrics.Plan
		plan, verr = metrics.ValidateIn(q, r.Clock.Now().UTC(), zone)
		if verr == nil {
			if plan.ReadsUsage() {
				if err := r.State.RequireEventStore("a usage measure"); err != nil {
					writeRefusal(r, err, "metrics query refused")
					return
				}
			}
			res, err := svc.Execute(r.Request.Context(), plan)
			if err != nil {
				r.InternalError("metrics query failed", err)
				return
			}
			r.JSON(http.StatusOK, res)
			return
		}
	}
	r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, billing.CodeMetricsQueryInvalid, verr.Error()).
		WithMetadata(map[string]any{"errors": verr.Errors}))
}
