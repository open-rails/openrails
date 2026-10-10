package handlers

// The merchant's api_host (#850, #1107): the Host-header value public routes
// resolve the merchant from. Operators bind it through the merchant manifest
// or the server's SetMerchantAPIHost; a hosted product claims and proves one
// through the server's ClaimMerchantAPIHost and VerifyMerchantAPIHost.

import (
	"errors"
	"net/http"

	"github.com/open-rails/openrails/billing"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
)

func apiHostMerchantScope(r *httprequest.Request) (billing.MerchantID, bool) {
	mid, ok := merchant.FromContext(r.Request.Context())
	if !ok || mid.IsZero() {
		r.ErrorCode(billing.CodeMerchantUnresolved, "")
		return billing.MerchantID{}, false
	}
	if r.State == nil || r.State.Merchants == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "merchant directory service unavailable")
		return billing.MerchantID{}, false
	}
	return mid, true
}

// writeAPIHostError maps a directory read's errors onto their refusals.
func writeAPIHostError(r *httprequest.Request, err error) {
	if errors.Is(err, merchants.ErrMerchantNotFound) {
		r.ErrorCode(billing.CodeResourceNotFound, "merchant not found")
		return
	}
	r.InternalError("api_host read failed", err)
}

// GetMerchantAPIHost handles GET /v1/admin/api-host: the proven api_host
// (null when unset) and the open claim, if any.
func GetMerchantAPIHost(r *httprequest.Request) {
	mid, ok := apiHostMerchantScope(r)
	if !ok {
		return
	}
	ctx := r.Request.Context()
	cfg, err := r.State.Merchants.GetHostConfig(ctx, mid)
	if err != nil {
		writeAPIHostError(r, err)
		return
	}
	claim, err := r.State.Merchants.APIHostClaimOf(ctx, mid)
	if err != nil {
		writeAPIHostError(r, err)
		return
	}
	r.JSON(http.StatusOK, merchants.APIHostView(cfg.APIHost, claim))
}
