package handlers

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/copilot"
	"github.com/open-rails/openrails/pkg/merchant"
)

// CatalogCopilotAsk handles POST /v1/merchant/catalog/ask (#779): the
// console catalog copilot. Phase 1 (read-only Q&A over catalog/pricing/
// reprice data) always runs when configured; Phase 2's draft_price_change /
// draft_catalog_diff tools are additionally present ONLY when
// llm.catalog_drafting_enabled is set (flag-off = absent from the tool list,
// never present-but-erroring). The model never mutates — its only
// write-shaped output is a DRAFT for human review in the console.
func CatalogCopilotAsk(r *httprequest.Request) {
	svc := r.State.CopilotService
	if svc == nil {
		r.ErrorJSON(http.StatusServiceUnavailable, "catalog copilot service not configured")
		return
	}
	if !svc.Configured() {
		r.ErrorJSON(http.StatusNotImplemented,
			"the catalog copilot is not configured on this deployment: set llm.api_key (env LLM_API_KEY) AND llm.catalog_copilot_enabled (env LLM_CATALOG_COPILOT_ENABLED=true) — see docs/admin-console.md")
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
		var limited *copilot.RateLimitedError
		var noAnswer *copilot.NoAnswerError
		switch {
		case errors.As(err, &limited):
			r.SetHeader("Retry-After", strconv.Itoa(int(math.Ceil(limited.RetryAfter.Seconds()))))
			r.ErrorJSON(http.StatusTooManyRequests, "catalog copilot rate limit exceeded — try again later")
		case errors.As(err, &noAnswer):
			r.ErrorJSON(http.StatusBadGateway, "the model did not produce an answer within the query budget — try a narrower question")
		default:
			r.ErrorJSON(http.StatusBadGateway, "catalog copilot failed: the LLM request did not complete")
		}
		return
	}
	r.JSON(http.StatusOK, res)
}

// CatalogCopilotConfirmDraft handles POST /v1/merchant/catalog/copilot/confirm
// (#779): the console calls this immediately after a human confirms a
// copilot-drafted price change / catalog diff through the normal wizard/
// create-price flow — a pure audit-provenance log entry (drafted-by-copilot
// marker), never a mutation itself (the confirm already happened via the
// normal catalog/reprice endpoints). Best-effort: logging failure never
// blocks the console (the mutation already succeeded by the time this is
// called).
func CatalogCopilotConfirmDraft(r *httprequest.Request) {
	var body struct {
		DraftID  string `json:"draft_id"`
		Kind     string `json:"kind"`
		PriceKey string `json:"price_key,omitempty"`
	}
	if err := json.NewDecoder(r.Request.Body).Decode(&body); err != nil || strings.TrimSpace(body.DraftID) == "" || strings.TrimSpace(body.Kind) == "" {
		r.ErrorJSON(http.StatusBadRequest, `body must be {"draft_id":"...","kind":"price_change"|"catalog_diff","price_key":"..."}`)
		return
	}
	ctx := r.Request.Context()
	fields := log.Fields{
		"draft_id":   body.DraftID,
		"kind":       body.Kind,
		"price_key":  body.PriceKey,
		"drafted_by": copilot.DraftedBy,
	}
	if mid, err := merchant.Require(ctx); err == nil {
		fields["merchant_id"] = mid.String()
	}
	log.WithContext(ctx).WithFields(fields).Info("copilot draft confirmed by human")
	r.SuccessJSONMessage("draft confirmation logged")
}
