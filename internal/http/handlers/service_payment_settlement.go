package handlers

import (
	"net/http"

	"github.com/open-rails/openrails"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/service"
)

func ServicePaymentSettlementStatus(r *httprequest.Request) {
	customerID, err := openrails.ParseCustomerID(r.Param("customer_id"))
	if err != nil || customerID.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "customer id is required")
		return
	}
	priceID, err := openrails.ParsePriceID(r.Query("price_id"))
	if err != nil || priceID.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "price id is required")
		return
	}
	svc, err := service.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "payment settlement status unavailable")
		return
	}
	settled, err := svc.HasSettledPayment(r.Request.Context(), customerID.UUID(), priceID.UUID())
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "payment settlement status unavailable")
		return
	}
	r.JSON(http.StatusOK, map[string]bool{"settled": settled})
}
