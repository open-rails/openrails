package handlers

import (
	"errors"
	"net/http"

	"github.com/open-rails/openrails"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	service "github.com/open-rails/openrails/internal/service"
)

// ServiceEnsureCustomer handles PUT /v1/merchant/customers/{customer_id}:
// materialize or touch the credential merchant's customer record.
func ServiceEnsureCustomer(r *httprequest.Request) {
	customerID, err := openrails.ParseCustomerID(r.Param("customer_id"))
	if err != nil || customerID.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid customer_id")
		return
	}
	id := customerID.UUID()
	svc, err := service.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	customer, err := svc.EnsureCustomer(r.Request.Context(), id)
	if err != nil {
		var status *openrails.StatusError
		if errors.As(err, &status) {
			r.JSON(status.Status, map[string]any{"error": status.ErrorDetails})
			return
		}
		r.ErrorJSON(http.StatusInternalServerError, "failed to ensure customer")
		return
	}
	r.JSON(http.StatusOK, customer)
}
