package handlers

import (
	"errors"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/copilot"
)

// AskCatalog handles POST /v1/merchant/catalog/ask: a model answers a
// question about the catalog from read-only lookups and, when drafting is
// enabled, proposes price changes for a person to review. It changes nothing.
func AskCatalog(r *httprequest.Request) {
	svc := r.State.CopilotService
	if !svc.Configured() {
		r.ErrorCode(billing.CodeServiceUnavailable, "catalog Q&A unavailable")
		return
	}
	var body billing.AskCatalogParams
	if !r.BindJSON(&body) {
		return
	}
	question := strings.TrimSpace(body.Question)
	if question == "" {
		r.APIError(api.Coded(billing.CodeInvalidParam, "question is required").WithParam("question"))
		return
	}
	res, err := svc.Ask(r.Request.Context(), question)
	if err != nil {

		var noAnswer *copilot.NoAnswerError
		modelFailure(r, err, errors.As(err, &noAnswer))
		return
	}
	r.SuccessJSON(res)
}

// modelFailure answers a question or prompt the language model did not
// complete; noAnswer is a model that spent its lookups without answering.
func modelFailure(r *httprequest.Request, err error, noAnswer bool) {
	message := "the model request did not complete"
	if noAnswer {
		message = "the model did not answer within its lookup budget; ask a narrower question"
	}
	log.WithContext(r.Request.Context()).WithError(err).Warn("model request failed")
	r.ErrorCode(billing.CodeModelUnavailable, message)
}
