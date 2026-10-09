package handlers

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/service"
)

// GetPaymentSettlementStatus (GET /admin/customers/{customer_id}/payment-settlement-status?price_id=)
// reports whether the customer ever paid for the price through a rail.
func GetPaymentSettlementStatus(r *httprequest.Request) {
	customerID, err := billing.ParseCustomerID(r.Param("customer_id"))
	if err != nil || customerID.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "customer_id is invalid").WithParam("customer_id"))
		return
	}
	priceID, err := billing.ParsePriceID(r.Query("price_id"))
	if err != nil || priceID.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidQuery, "price_id is required").WithParam("price_id"))
		return
	}
	svc, err := service.New(r.State)
	if err != nil {
		r.InternalError("payment settlement status unavailable", err)
		return
	}
	settled, err := svc.HasSettledPayment(r.Request.Context(), customerID.UUID(), priceID.UUID())
	if err != nil {
		r.InternalError("payment settlement status unavailable", err)
		return
	}
	r.SuccessJSON(billing.PaymentSettlementStatus{Settled: settled})
}
