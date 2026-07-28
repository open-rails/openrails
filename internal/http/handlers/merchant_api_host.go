package handlers

// Merchant api_host operator surface (#850): read + assign the merchant's
// canonical #734 API host (the Host-header value public routes resolve the
// merchant from) without SQL. The write is gated on merchant:settings:update —
// owner-only in the fixed #567 catalog. MODE 1 deployments declare `api_host`
// in the merchant manifest instead; this route and the manifest write the same
// merchants.Service.SetHostConfig seam the saas wrapper's assign-api-host
// action uses (engine.SetMerchantAPIHost -> SetHostConfig).

import (
	"errors"
	"net/http"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/merchant"
)

func apiHostMerchantScope(r *httprequest.Request) (merchant.ID, bool) {
	mid, ok := merchant.FromContext(r.Request.Context())
	if !ok || mid.IsZero() {
		r.ErrorJSON(http.StatusForbidden, "merchant_unresolved")
		return merchant.ID{}, false
	}
	if r.State == nil || r.State.Merchants == nil {
		r.ErrorJSON(http.StatusNotImplemented, "merchant directory service is not armed on this deployment")
		return merchant.ID{}, false
	}
	return mid, true
}

func apiHostResponse(host string) map[string]any {
	resp := map[string]any{"api_host": nil}
	if host != "" {
		resp["api_host"] = host
	}
	return resp
}

// GetMerchantAPIHost handles GET /v1/merchant/api-host: the caller's merchant's
// canonical API host, null when unset.
func GetMerchantAPIHost(r *httprequest.Request) {
	mid, ok := apiHostMerchantScope(r)
	if !ok {
		return
	}
	cfg, err := r.State.Merchants.GetHostConfig(r.Request.Context(), mid)
	if err != nil {
		if errors.Is(err, merchants.ErrMerchantNotFound) {
			r.ErrorJSON(http.StatusNotFound, "merchant not found")
			return
		}
		r.ErrorJSON(http.StatusInternalServerError, "read api_host failed")
		return
	}
	r.JSON(http.StatusOK, apiHostResponse(cfg.APIHost))
}

// PutMerchantAPIHost handles PUT /v1/merchant/api-host {"api_host": …}:
// assigns the caller's merchant's canonical API host (bare lowercase hostname;
// "" clears the mapping). The very next request against the new Host resolves
// immediately on every node (#734 — live directory row, no boot map). 409 when
// the host is already assigned to a different active merchant.
func PutMerchantAPIHost(r *httprequest.Request) {
	mid, ok := apiHostMerchantScope(r)
	if !ok {
		return
	}
	var req struct {
		APIHost string `json:"api_host"`
	}
	if !r.BindJSON(&req) {
		return
	}
	host := merchants.NormalizeAPIHost(req.APIHost)
	if host != "" {
		if err := merchants.ValidateAPIHost(host); err != nil {
			r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "invalid_api_host",
				"api_host must be a bare lowercase hostname (no scheme, port, or path), e.g. api.myapp.example"))
			return
		}
	}
	if err := r.State.Merchants.SetHostConfig(r.Request.Context(), mid, host); err != nil {
		switch {
		case errors.Is(err, merchants.ErrAPIHostTaken):
			r.APIError(api.NewAPIError(http.StatusConflict, api.ErrorTypeInvalidRequest, "api_host_taken",
				"that api_host is already assigned to another merchant"))
		case errors.Is(err, merchants.ErrInvalidAPIHost):
			r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, "invalid_api_host",
				"api_host must be a bare lowercase hostname (no scheme, port, or path), e.g. api.myapp.example"))
		case errors.Is(err, merchants.ErrMerchantNotFound):
			r.ErrorJSON(http.StatusNotFound, "merchant not found")
		default:
			r.ErrorJSON(http.StatusInternalServerError, "api_host assignment failed")
		}
		return
	}
	r.JSON(http.StatusOK, apiHostResponse(host))
}
