package handlers

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/dashboard"
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

// MerchantMetricsAsk handles POST /v1/merchant/metrics/ask (#756): a free-form
// question answered by an LLM that runs compiler-validated metrics queries as
// tools on the caller's RLS-pinned merchant context. UNLIKE widget generation
// (#741, schema-only), the model sees aggregate query RESULTS — so this is
// gated on the separate llm.ask_enabled consent (fail-closed 501) and
// rate-limited per merchant. The response carries the model's answer plus the
// VERBATIM result of every executed query as evidence.
func MerchantMetricsAsk(r *httprequest.Request) {
	svc := r.State.DashboardService
	if svc == nil {
		r.ErrorJSON(http.StatusServiceUnavailable, "dashboard service not configured")
		return
	}
	if !svc.AskConfigured() {
		if !svc.NLConfigured() {
			r.ErrorJSON(http.StatusNotImplemented,
				"metrics Q&A is not configured on this deployment: set llm.api_key (env LLM_API_KEY) AND llm.ask_enabled (env LLM_ASK_ENABLED=true) — see docs/admin-console.md")
			return
		}
		r.ErrorJSON(http.StatusNotImplemented,
			"metrics Q&A is disabled: unlike widget generation, /ask sends aggregate query RESULTS to the LLM provider — set llm.ask_enabled (env LLM_ASK_ENABLED=true) to consent; see docs/admin-console.md")
		return
	}
	var body struct {
		Question string `json:"question"`
	}
	if err := json.NewDecoder(r.Request.Body).Decode(&body); err != nil || strings.TrimSpace(body.Question) == "" {
		r.ErrorJSON(http.StatusBadRequest, `body must be {"question":"<what you want to know>"}`)
		return
	}
	res, err := svc.Ask(r.Request.Context(), strings.TrimSpace(body.Question))
	if err != nil {
		var limited *dashboard.AskRateLimitedError
		var noAnswer *dashboard.AskNoAnswerError
		switch {
		case errors.As(err, &limited):
			r.SetHeader("Retry-After", strconv.Itoa(int(math.Ceil(limited.RetryAfter.Seconds()))))
			r.ErrorJSON(http.StatusTooManyRequests, "ask rate limit exceeded — try again later")
		case errors.As(err, &noAnswer):
			r.ErrorJSON(http.StatusBadGateway, "the model did not produce an answer within the query budget — try a narrower question")
		default:
			r.ErrorJSON(http.StatusBadGateway, "metrics Q&A failed: the LLM request did not complete")
		}
		return
	}
	r.JSON(http.StatusOK, res)
}
