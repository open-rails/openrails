package handlers

import (
	"github.com/open-rails/openrails/billing"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// ListMandates is one page of a customer's mandates, newest first: the
// stored-credential agreements on their cards and the references each sends.
func ListMandates(r *httprequest.Request) {
	customer, ok := commerceCustomer(r, customerIDParam(r.Param("customer_id")))
	if !ok {
		return
	}
	ids, ok := listIDs(r, billing.ParseMandateID)
	if !ok {
		return
	}
	page, ok := r.Page()
	if !ok {
		return
	}
	ctx := r.Request.Context()
	mid, err := merchant.Require(ctx)
	if err != nil {
		writeRefusal(r, err, "failed to list mandates")
		return
	}
	var out billing.ListPage[billing.Mandate]
	if ids != nil {
		out, err = mandates.ListByIDs(ctx, r.State.DB.Gen(ctx), mid.UUID(), customer.UUID(), uuidutil.Of(ids))
	} else {
		out, err = mandates.List(ctx, r.State.DB.Gen(ctx), mid.UUID(), customer.UUID(), page)
	}
	if err != nil {
		writeRefusal(r, err, "failed to list mandates")
		return
	}
	r.SuccessJSON(out)
}
