package handlers

// Merchant api_host surface (#850, #1107): the Host-header value public routes
// resolve the merchant from. Owner-only (merchant:settings:update). A merchant
// claims a host, publishes the claim's token in a TXT record at
// _openrails-challenge.<host>, and verifies; only a proven host routes. The
// deployment's own hosts (Runtime.ReservedAPIHosts) are never claimable.
// Operators bind hosts directly through the merchant manifest or
// ControlPlane.SetMerchantAPIHost.

import (
	"errors"
	"net/http"

	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

func apiHostMerchantScope(r *httprequest.Request) (merchant.ID, bool) {
	mid, ok := merchant.FromContext(r.Request.Context())
	if !ok || mid.IsZero() {
		r.ErrorJSON(http.StatusForbidden, "merchant_unresolved")
		return merchant.ID{}, false
	}
	if r.State == nil || r.State.Merchants == nil {
		r.ErrorJSON(http.StatusServiceUnavailable, "merchant directory service unavailable")
		return merchant.ID{}, false
	}
	return mid, true
}

func apiHostResponse(host string, claim *merchants.APIHostClaim) map[string]any {
	resp := map[string]any{"api_host": nil, "claim": nil}
	if host != "" {
		resp["api_host"] = host
	}
	if claim != nil {
		resp["claim"] = map[string]any{
			"api_host":   claim.APIHost,
			"created_at": claim.CreatedAt,
			"dns_record": map[string]string{"type": "TXT", "name": claim.Record(), "value": claim.Token},
		}
	}
	return resp
}

// writeAPIHostError maps the api_host errors onto their refusals.
func writeAPIHostError(r *httprequest.Request, err error, claim *merchants.APIHostClaim) {
	refuse := func(status int, code, message string) {
		r.APIError(api.NewAPIError(status, api.ErrorTypeInvalidRequest, code, message))
	}
	switch {
	case errors.Is(err, merchants.ErrInvalidAPIHost):
		refuse(http.StatusBadRequest, "invalid_api_host",
			"api_host must be a bare lowercase domain name (no scheme, port, path or address), e.g. api.myapp.example")
	case errors.Is(err, merchants.ErrAPIHostReserved):
		refuse(http.StatusBadRequest, "api_host_reserved", "that api_host serves this deployment; use a host of your own")
	case errors.Is(err, merchants.ErrAPIHostTaken):
		refuse(http.StatusConflict, "api_host_taken", "that api_host is already assigned to another merchant")
	case errors.Is(err, merchants.ErrAPIHostClaimMissing):
		refuse(http.StatusConflict, "api_host_claim_missing", "claim an api_host with PUT /v1/merchant/api-host first")
	case errors.Is(err, merchants.ErrAPIHostUnproven):
		message := "the challenge record does not carry the claim's token yet"
		if claim != nil {
			message = "publish a TXT record at " + claim.Record() + " with value " + claim.Token + ", then verify again"
		}
		refuse(http.StatusConflict, "api_host_unproven", message)
	case errors.Is(err, merchants.ErrMerchantNotFound):
		r.ErrorJSON(http.StatusNotFound, "merchant not found")
	default:
		r.InternalError("api_host change failed", err)
	}
}

// GetMerchantAPIHost handles GET /v1/merchant/api-host: the proven api_host
// (null when unset) and the open claim, if any.
func GetMerchantAPIHost(r *httprequest.Request) {
	mid, ok := apiHostMerchantScope(r)
	if !ok {
		return
	}
	ctx := r.Request.Context()
	cfg, err := r.State.Merchants.GetHostConfig(ctx, mid)
	if err != nil {
		writeAPIHostError(r, err, nil)
		return
	}
	claim, err := r.State.Merchants.APIHostClaimOf(ctx, mid)
	if err != nil {
		writeAPIHostError(r, err, nil)
		return
	}
	r.JSON(http.StatusOK, apiHostResponse(cfg.APIHost, claim))
}

// PutMerchantAPIHost handles PUT /v1/merchant/api-host {"api_host": …}. A new
// host opens a claim (202) that routes nothing until verified; "" releases
// the api_host and any claim at once; the current host is a no-op.
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
	ctx := r.Request.Context()
	host := merchants.NormalizeAPIHost(req.APIHost)
	if host == "" {
		if err := r.State.Merchants.ReleaseAPIHost(ctx, mid); err != nil {
			writeAPIHostError(r, err, nil)
			return
		}
		r.JSON(http.StatusOK, apiHostResponse("", nil))
		return
	}
	cfg, err := r.State.Merchants.GetHostConfig(ctx, mid)
	if err != nil {
		writeAPIHostError(r, err, nil)
		return
	}
	if cfg.APIHost == host {
		r.JSON(http.StatusOK, apiHostResponse(host, nil))
		return
	}
	if err := merchants.ClaimableAPIHost(host, r.State.ReservedAPIHosts); err != nil {
		writeAPIHostError(r, err, nil)
		return
	}
	claim, err := r.State.Merchants.ClaimAPIHost(ctx, mid, host)
	if err != nil {
		writeAPIHostError(r, err, nil)
		return
	}
	r.JSON(http.StatusAccepted, apiHostResponse(cfg.APIHost, claim))
}

// VerifyMerchantAPIHost handles POST /v1/merchant/api-host/verify: proves the
// open claim through DNS and binds its host.
func VerifyMerchantAPIHost(r *httprequest.Request) {
	mid, ok := apiHostMerchantScope(r)
	if !ok {
		return
	}
	ctx := r.Request.Context()
	host, err := r.State.Merchants.VerifyAPIHost(ctx, mid, r.State.ReservedAPIHosts, r.State.DNSResolver)
	if err != nil {
		claim, _ := r.State.Merchants.APIHostClaimOf(ctx, mid)
		writeAPIHostError(r, err, claim)
		return
	}
	r.JSON(http.StatusOK, apiHostResponse(host, nil))
}
