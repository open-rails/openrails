package handlers

import (
	"net/http"

	"github.com/google/uuid"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/pkg/service"
)

func ServicePaymentSettlementStatus(r *httprequest.Request) {
	customerID, err := uuid.Parse(r.Param("customer_id"))
	if err != nil || customerID == uuid.Nil {
		r.ErrorJSON(http.StatusBadRequest, "customer id is required")
		return
	}
	priceID, err := uuid.Parse(r.Query("price_id"))
	if err != nil || priceID == uuid.Nil {
		r.ErrorJSON(http.StatusBadRequest, "price id is required")
		return
	}
	svc, err := service.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "payment settlement status unavailable")
		return
	}
	settled, err := svc.HasSettledPayment(r.Request.Context(), customerID, priceID)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "payment settlement status unavailable")
		return
	}
	r.JSON(http.StatusOK, map[string]bool{"settled": settled})
}
