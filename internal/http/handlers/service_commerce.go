package handlers

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

// commerceCustomer requires a customer id and the caller's scope over it.
func commerceCustomer(r *httprequest.Request, customerID billing.CustomerID) (identity.CustomerID, bool) {
	id := servicePayer(customerID)
	if id == nil {
		r.APIError(api.Coded(billing.CodeInvalidParam, "valid customer_id required").WithParam("customer_id"))
		return identity.CustomerID{}, false
	}
	if !requireServiceCustomerScope(r, *id) {
		return identity.CustomerID{}, false
	}
	return *id, true
}
