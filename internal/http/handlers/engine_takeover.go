package handlers

import (
	"errors"
	"net/http"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/pkg/api"
)

func PreviewEngineTakeover(r *httprequest.Request) { engineTakeover(r, "preview") }
func EngineTakeover(r *httprequest.Request)        { engineTakeover(r, "submit") }
func GetEngineTakeover(r *httprequest.Request)     { engineTakeover(r, "get") }
func AbandonEngineTakeover(r *httprequest.Request) { engineTakeover(r, "abandon") }

func engineTakeover(r *httprequest.Request, action string) {
	ctx := r.Request.Context()
	sid, err := openrails.ParseSubscriptionID(r.Param("id"))
	if err != nil || sid.IsZero() {
		cutoverRefusal(r, http.StatusBadRequest, "invalid_param", "invalid subscription ID", "subscription_id")
		return
	}
	h := r.State.EngineTakeovers
	if h == nil {
		cutoverRefusal(r, http.StatusServiceUnavailable, "engine_takeover_unavailable", "engine takeover unavailable", "")
		return
	}
	var out *openrails.EngineTakeover
	switch action {
	case "preview":
		out, err = h.Preview(ctx, sid.UUID())
	case "get":
		out, err = h.Get(ctx, sid.UUID())
	case "abandon":
		out, err = h.Abandon(ctx, sid.UUID())
	default:
		out, err = h.Submit(ctx, r.State.IntentRunner(), sid.UUID(), r.Request.Header.Get("Idempotency-Key"), intents.OriginAdmin)
	}
	if err != nil {
		writeEngineTakeoverError(r, err)
		return
	}
	status := http.StatusOK
	if action == "submit" && out.Status != intents.StatusSucceeded && out.Status != intents.StatusFailedTerminal {
		status = http.StatusAccepted
	}
	r.JSON(status, out)
}

// EngineTakeoverBatch admits takeovers in bulk.
func EngineTakeoverBatch(r *httprequest.Request) {
	if r.State.EngineTakeovers == nil {
		cutoverRefusal(r, http.StatusServiceUnavailable, "engine_takeover_unavailable", "engine takeover unavailable", "")
		return
	}
	var body openrails.EngineTakeoverBatchRequest
	if !r.BindJSON(&body) {
		return
	}
	out, err := r.State.EngineTakeovers.Batch(r.Request.Context(), r.State.IntentRunner(), body)
	if err != nil {
		writeEngineTakeoverError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

func writeEngineTakeoverError(r *httprequest.Request, err error) {
	var refusal *intents.EngineTakeoverRefusal
	switch {
	case errors.As(err, &refusal):
		cutoverRefusal(r, refusal.Status, refusal.Code, refusal.Message, "")
	case errors.Is(err, openrails.ErrInvalid):
		cutoverRefusal(r, http.StatusBadRequest, "invalid_param", "canonical idempotency key of 1..200 bytes required", "idempotency_key")
	case errors.Is(err, intents.ErrRateCeilingTripped):
		r.APIError(api.NewAPIError(http.StatusTooManyRequests, api.ErrorTypeRateLimit, api.CodeRateLimitExceeded, "Destructive operation rate limit reached; try again later"))
	case db.IsNotFound(err):
		cutoverRefusal(r, http.StatusNotFound, "resource_missing", "subscription not found", "subscription_id")
	default:
		r.InternalError("engine takeover failed", err)
	}
}
