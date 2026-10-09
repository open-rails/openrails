package handlers

import (
	"net/http"

	"github.com/open-rails/openrails/billing"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/scim"
)

// provisioningTokens is the merchant's tokens and the merchant the request
// acts on.
func provisioningTokens(r *httprequest.Request) (scim.Tokens, billing.MerchantID, bool) {
	mid, err := merchant.Require(r.Request.Context())
	if err != nil {
		writeRefusal(r, err, "merchant unresolved")
		return scim.Tokens{}, mid, false
	}
	return scim.Tokens{DB: r.State.DB}, mid, true
}

// ListProvisioningTokens handles GET /v1/admin/provisioning-tokens.
func ListProvisioningTokens(r *httprequest.Request) {
	ids, ok := listIDs(r, billing.ParseProvisioningTokenID)
	if !ok {
		return
	}
	tokens, mid, ok := provisioningTokens(r)
	if !ok {
		return
	}
	list, err := tokens.List(r.Request.Context(), mid, ids)
	if err != nil {
		writeRefusal(r, err, "list provisioning tokens failed")
		return
	}
	r.SuccessJSON(billing.ListPage[billing.ProvisioningToken]{Items: list})
}

// CreateProvisioningToken handles POST /v1/admin/provisioning-tokens.
func CreateProvisioningToken(r *httprequest.Request) {
	var in billing.CreateProvisioningTokenParams
	if !r.BindJSON(&in) {
		return
	}
	tokens, mid, ok := provisioningTokens(r)
	if !ok {
		return
	}
	created, err := tokens.Create(r.Request.Context(), mid, in.Name)
	if err != nil {
		writeRefusal(r, err, "create provisioning token failed")
		return
	}
	r.JSON(http.StatusCreated, created)
}

// DeleteProvisioningToken handles DELETE /v1/admin/provisioning-tokens/{id}.
func DeleteProvisioningToken(r *httprequest.Request) {
	id, ok := pathID(r, billing.ParseProvisioningTokenID)
	if !ok {
		return
	}
	tokens, mid, ok := provisioningTokens(r)
	if !ok {
		return
	}
	if err := tokens.Delete(r.Request.Context(), mid, id); err != nil {
		writeRefusal(r, err, "revoke provisioning token failed")
		return
	}
	r.NoContent()
}
