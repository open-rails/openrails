package handlers

import (
	"github.com/open-rails/openrails/billing"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

// ListMyAgreements handles GET /v1/me/agreements: the customer's live charge
// agreements, newest first.
func ListMyAgreements(r *httprequest.Request) {
	customer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	page, ok := r.Page()
	if !ok {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	out, err := svc.ListAgreements(r.Request.Context(), billing.CustomerID(customer), page)
	if err != nil {
		writeRefusal(r, err, "listing agreements failed")
		return
	}
	r.SetHeader("Cache-Control", "no-store")
	r.SuccessJSON(out)
}

// RevokeMyAgreement handles POST /v1/me/agreements/{id}/revoke: the
// merchant charges the card under it no more.
func RevokeMyAgreement(r *httprequest.Request) {
	customer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	id, err := billing.ParseMandateID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.ErrorCode(billing.CodeInvalidParam, "id is not an agreement id")
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	out, err := svc.RevokeAgreement(r.Request.Context(), billing.CustomerID(customer), id)
	if err != nil {
		writeRefusal(r, err, "revoking the agreement failed")
		return
	}
	r.SuccessJSON(out)
}
