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

// MerchantMetricsSchema handles GET /v1/merchant/metrics/schema: the registry
// dump (measures + formulas + dims + caveats + examples) —
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
		r.ErrorCode(billing.CodeServiceUnavailable, "metrics service not configured")
		return
	}
	q, verr := metrics.DecodeQuery(r.Request.Body)
	if verr == nil {
		var plan *metrics.Plan
		plan, verr = metrics.Validate(q)
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

// MerchantMetricsAsk handles POST /v1/merchant/metrics/ask (#756): a free-form
// question answered by an LLM that runs compiler-validated metrics queries as
// tools on the caller's merchant-scoped context. UNLIKE widget generation
// (#741, schema-only), the model sees aggregate query RESULTS — so this is
// registered only with the separate llm.ask_enabled consent and
// protected by the shared route abuse limiter. The response carries the model's answer plus the
// VERBATIM result of every executed query as evidence.
func MerchantMetricsAsk(r *httprequest.Request) {
	svc := r.State.DashboardService
	if !svc.AskConfigured() {
		r.ErrorCode(billing.CodeServiceUnavailable, "metrics Q&A unavailable")
		return
	}
	var body billing.AskMetricsParams
	if !r.BindJSON(&body) {
		return
	}
	if strings.TrimSpace(body.Question) == "" {
		r.APIError(api.Coded(billing.CodeInvalidParam, "question is required").WithParam("question"))
		return
	}
	res, err := svc.Ask(r.Request.Context(), strings.TrimSpace(body.Question))
	if err != nil {

		var noAnswer *dashboard.AskNoAnswerError
		modelFailure(r, err, errors.As(err, &noAnswer))
		return
	}
	r.JSON(http.StatusOK, res)
}
