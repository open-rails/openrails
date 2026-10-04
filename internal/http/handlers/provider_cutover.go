package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/db"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/providerqualification"
)

func ProviderCutover(r *httprequest.Request)        { providerCutover(r, false) }
func PreviewProviderCutover(r *httprequest.Request) { providerCutover(r, true) }

func providerCutover(r *httprequest.Request, preview bool) {
	ctx := r.Request.Context()
	sid, err := billing.ParseSubscriptionID(r.Param("id"))
	if err != nil || sid.IsZero() {
		cutoverRefusal(r, http.StatusBadRequest, "invalid_param", "invalid subscription ID", "subscription_id")
		return
	}
	id := sid.UUID()
	if r.State.ProviderCutovers == nil {
		cutoverRefusal(r, http.StatusServiceUnavailable, "provider_cutover_unavailable", "provider cutover unavailable", "")
		return
	}
	if !preview {
		key := r.Request.Header.Get("Idempotency-Key")
		if r.Request.Method == http.MethodGet {
			key = r.Query("idempotency_key")
		}
		if key == "" || key != strings.TrimSpace(key) || len(key) > 255 || strings.ContainsAny(key, "\r\n\t") {
			cutoverRefusal(r, 400, "invalid_param", "canonical idempotency key of 1..255 bytes required", "idempotency_key")
			return
		}
	}
	var out *billing.ProviderCutover
	if r.Request.Method == http.MethodGet {
		out, err = r.State.ProviderCutovers.Get(ctx, id, r.Query("idempotency_key"))
	} else {
		var body billing.ProviderCutoverRequest
		if !r.BindJSON(&body) {
			return
		}
		param := ""
		if body.TargetPaymentMethodID.IsZero() {
			param = "target_payment_method_id"
		} else if body.ExpectedSourcePSPID == uuid.Nil {
			param = "expected_source_psp_id"
		} else if body.ExpectedTargetPSPID == uuid.Nil {
			param = "expected_target_psp_id"
		}
		if param != "" {
			cutoverRefusal(r, 400, "invalid_param", "valid nonzero identifier required", param)
			return
		}
		if preview {
			out, err = r.State.ProviderCutovers.Preview(ctx, id, body)
		} else {
			out, err = r.State.ProviderCutovers.Submit(ctx, r.State.IntentRunner(), id, r.Request.Header.Get("Idempotency-Key"), body, intents.OriginAdmin)
		}
	}
	if err != nil {
		switch {
		case errors.Is(err, billing.ErrInvalid):
			cutoverRefusal(r, 400, "invalid_param", "invalid cutover request", "")
		case errors.Is(err, providerqualification.ErrUnqualified), errors.Is(err, providerqualification.ErrInvalid):
			cutoverRefusal(r, http.StatusConflict, "provider_cutover_unqualified", "both provider accounts require explicit cutover qualification", "")
		case errors.Is(err, intents.ErrProviderCutoverConflict):
			cutoverRefusal(r, http.StatusConflict, "provider_cutover_conflict", err.Error(), "")
		case errors.Is(err, intents.ErrRateCeilingTripped):
			r.APIError(api.NewAPIError(http.StatusTooManyRequests, api.ErrorTypeRateLimit, api.CodeRateLimitExceeded,
				"Destructive operation rate limit reached; try again later or contact support"))
		case db.IsNotFound(err), errors.Is(err, providerqualification.ErrNotFound):
			cutoverRefusal(r, http.StatusNotFound, api.CodeResourceNotFound, "cutover or subscription not found", "")
		default:
			cutoverRefusal(r, http.StatusInternalServerError, api.CodeInternalError, "provider cutover failed", "")
		}
		return
	}
	status := http.StatusOK
	if !preview && r.Request.Method != http.MethodGet && out.Status != "succeeded" && out.Status != "failed_terminal" && out.Status != "superseded" && out.Status != "expired" {
		status = http.StatusAccepted
	}
	r.JSON(status, out)
}

func cutoverRefusal(r *httprequest.Request, status int, code, message, param string) {
	refusal := api.NewAPIError(status, api.ErrorTypeForStatus(status), code, message)
	if param != "" {
		refusal.WithParam(param)
	}
	r.APIError(refusal)
}
