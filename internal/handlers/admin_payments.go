package handlers

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/processors"
	"github.com/open-rails/openrails/internal/services"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/query"
)

// PaymentPath is the path parameter for single payment operations
type PaymentPath struct {
	PaymentID string `uri:"id" binding:"required"`
}

// RefundRequest is the request body for refunding a payment
type RefundRequest struct {
	Amount int64  `json:"amount" binding:"required,gt=0"` // Amount in cents to refund
	Reason string `json:"reason,omitempty"`               // Optional: duplicate, fraudulent, requested_by_customer
}

// AdminRefundPayment issues a refund for a payment through the processor and records it
// POST /v1/admin/payments/:id/refund
//
// For NMI/Mobius and Stripe: Issues the refund through the processor API
// For CCBill: Returns an error - refunds must be done through CCBill's admin portal
func AdminRefundPayment(r *httprequest.Request) {
	var path PaymentPath
	if err := r.Inner().ShouldBindUri(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}

	paymentID, err := api.ParsePaymentID(path.PaymentID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid payment ID")
		return
	}

	var req RefundRequest
	if !r.BindJSON(&req) {
		return
	}

	if r.State.PaymentService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "payment service unavailable")
		return
	}

	ctx := r.Request.Context()

	// Get the original payment
	payment, err := r.State.PaymentService.GetByID(ctx, paymentID)
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, "payment not found")
		return
	}

	// Issue refund through the appropriate processor
	var refundTransactionID string

	switch {
	case payment.Processor == models.ProcessorCCBill:
		// CCBill doesn't have an API for issuing refunds
		r.ErrorJSON(http.StatusBadRequest,
			"CCBill refunds must be processed through CCBill's admin portal. "+
				"After issuing the refund in CCBill, it will be recorded automatically via webhook.")
		return

	case payment.Processor == models.ProcessorStripe:
		refundTargetID, err := services.ResolveStripeRefundTarget(payment)
		if err != nil {
			r.ErrorJSON(http.StatusBadRequest, err.Error())
			return
		}

		// Issue refund through Stripe
		stripeService := &services.StripeRefundService{Config: r.State.Config}
		result, err := stripeService.CreateRefund(ctx, services.RefundParams{
			ChargeID: refundTargetID,
			Amount:   req.Amount,
			Reason:   req.Reason,
		})
		if err != nil {
			r.ErrorJSON(http.StatusBadGateway, fmt.Sprintf("stripe refund failed: %s", err.Error()))
			return
		}
		refundTransactionID = result.ID

	case processors.IsNMIBackedProcessor(payment.Processor):
		// Issue refund through NMI (Mobius, etc.)
		providerName := strings.ToLower(string(payment.Processor))
		nmiClient, ok := r.State.NMIClients[providerName]
		if !ok {
			r.ErrorJSON(http.StatusInternalServerError,
				fmt.Sprintf("NMI client not configured for processor: %s", payment.Processor))
			return
		}

		result, err := nmiClient.Refund(nmi.RefundParams{
			TransactionID: payment.TransactionID,
			Amount:        req.Amount,
		})
		if err != nil {
			r.ErrorJSON(http.StatusBadGateway, fmt.Sprintf("refund failed: %s", err.Error()))
			return
		}
		refundTransactionID = result.TransactionID

	default:
		r.ErrorJSON(http.StatusBadRequest,
			fmt.Sprintf("refunds not supported for processor: %s", payment.Processor))
		return
	}

	// Record the refund in the database
	refund, err := r.State.PaymentService.Refund(ctx, paymentID, refundTransactionID, req.Amount)
	if err != nil {
		// The refund was issued but we failed to record it - this is bad
		// Log the error with the refund transaction ID for manual recovery
		r.ErrorJSON(http.StatusInternalServerError,
			fmt.Sprintf("refund issued (ID: %s) but failed to record: %s", refundTransactionID, err.Error()))
		return
	}

	r.Inner().JSON(http.StatusCreated, PaymentToAPI(refund, nil))
}

// GetAdminPayments returns a paginated list of payments with filters
// GET /v1/admin/payments
func GetAdminPayments(r *httprequest.Request) {
	queryOpts := query.QueryOptions[services.GetPaymentsFilters]{}
	if err := r.Inner().ShouldBindQuery(&queryOpts); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}

	if r.State.PaymentService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "payment service unavailable")
		return
	}

	payments, total, err := r.State.PaymentService.GetPayments(r.Request.Context(), queryOpts)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}

	// Convert to PaymentObject list
	paymentObjects := make([]api.PaymentObject, len(payments))
	for i, p := range payments {
		paymentObjects[i] = PaymentToAPI(p, nil)
	}

	r.SuccessJSONPaginated(paymentObjects, total, queryOpts.Limit, queryOpts.Offset)
}

// GetAdminPayment returns a single payment with full details including refunds
// GET /v1/admin/payments/:id
func GetAdminPayment(r *httprequest.Request) {
	var path PaymentPath
	if err := r.Inner().ShouldBindUri(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}

	paymentID, err := api.ParsePaymentID(path.PaymentID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid payment ID")
		return
	}

	if r.State.PaymentService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "payment service unavailable")
		return
	}

	payment, refunds, err := r.State.PaymentService.GetByIDWithDetails(r.Request.Context(), paymentID)
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, "payment not found")
		return
	}

	r.SuccessJSON(PaymentToAPI(payment, refunds))
}
