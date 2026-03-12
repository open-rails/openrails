package handlers

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/processors"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/query"
)

type paymentPath struct {
	PaymentID string `uri:"id" binding:"required"`
}

type refundRequest struct {
	Amount int64  `json:"amount" binding:"required,gt=0"`
	Reason string `json:"reason,omitempty"`
}

type adminOffChannelPaymentPath struct {
	UserID string `uri:"user_id" binding:"required"`
}

type adminOffChannelPaymentRequest struct {
	PriceID          string         `json:"price_id" binding:"required"`
	TransactionID    string         `json:"transaction_id" binding:"required"`
	Amount           *int64         `json:"amount,omitempty"`
	Currency         string         `json:"currency,omitempty"`
	PurchasedAt      string         `json:"purchased_at,omitempty"`
	DiscountCode     *string        `json:"discount_code,omitempty"`
	DiscountReason   *string        `json:"discount_reason,omitempty"`
	DiscountMetadata map[string]any `json:"discount_metadata,omitempty"`
}

func AdminRefundPayment(r *httprequest.Request) {
	var path paymentPath
	if err := r.Inner().ShouldBindUri(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	paymentID, err := api.ParsePaymentID(path.PaymentID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid payment ID")
		return
	}
	var req refundRequest
	if !r.BindJSON(&req) {
		return
	}
	ctx := r.Request.Context()
	payment, err := r.State.PaymentService.GetByID(ctx, paymentID)
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, "payment not found")
		return
	}
	var refundTransactionID string
	switch {
	case payment.Processor == models.ProcessorCCBill:
		r.ErrorJSON(http.StatusBadRequest, "CCBill refunds must be processed through CCBill's admin portal. After issuing the refund in CCBill, it will be recorded automatically via webhook.")
		return
	case payment.Processor == models.ProcessorStripe:
		refundTargetID, err := subscriptions.ResolveStripeRefundTarget(payment)
		if err != nil {
			r.ErrorJSON(http.StatusBadRequest, err.Error())
			return
		}
		stripeService := &subscriptions.StripeRefundService{Config: r.State.Config}
		result, err := stripeService.CreateRefund(ctx, subscriptions.RefundParams{ChargeID: refundTargetID, Amount: req.Amount, Reason: req.Reason})
		if err != nil {
			r.ErrorJSON(http.StatusBadGateway, fmt.Sprintf("stripe refund failed: %s", err.Error()))
			return
		}
		refundTransactionID = result.ID
	case processors.IsNMIBackedProcessor(payment.Processor):
		providerName := strings.ToLower(string(payment.Processor))
		nmiClient, ok := r.State.NMIClients[providerName]
		if !ok {
			r.ErrorJSON(http.StatusInternalServerError, fmt.Sprintf("NMI client not configured for processor: %s", payment.Processor))
			return
		}
		result, err := nmiClient.Refund(nmi.RefundParams{TransactionID: payment.TransactionID, Amount: req.Amount})
		if err != nil {
			r.ErrorJSON(http.StatusBadGateway, fmt.Sprintf("refund failed: %s", err.Error()))
			return
		}
		refundTransactionID = result.TransactionID
	default:
		r.ErrorJSON(http.StatusBadRequest, fmt.Sprintf("refunds not supported for processor: %s", payment.Processor))
		return
	}
	refund, err := r.State.PaymentService.Refund(ctx, paymentID, refundTransactionID, req.Amount)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, fmt.Sprintf("refund issued (ID: %s) but failed to record: %s", refundTransactionID, err.Error()))
		return
	}
	r.Inner().JSON(http.StatusCreated, PaymentToAPI(refund, nil))
}

func GetAdminPayments(r *httprequest.Request) {
	queryOpts := query.QueryOptions[payments.GetPaymentsFilters]{}
	if err := r.Inner().ShouldBindQuery(&queryOpts); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	payments, total, err := r.State.PaymentService.GetPayments(r.Request.Context(), queryOpts)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}
	paymentObjects := make([]api.PaymentObject, len(payments))
	for i, p := range payments {
		paymentObjects[i] = PaymentToAPI(p, nil)
	}
	r.SuccessJSONPaginated(paymentObjects, total, queryOpts.Limit, queryOpts.Offset)
}

func GetAdminPayment(r *httprequest.Request) {
	var path paymentPath
	if err := r.Inner().ShouldBindUri(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	paymentID, err := api.ParsePaymentID(path.PaymentID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid payment ID")
		return
	}
	payment, refunds, err := r.State.PaymentService.GetByIDWithDetails(r.Request.Context(), paymentID)
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, "payment not found")
		return
	}
	r.SuccessJSON(PaymentToAPI(payment, refunds))
}

func GetAdminUserPayments(r *httprequest.Request) {
	var path adminUserPath
	if err := r.Inner().ShouldBindUri(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	page := 1
	pageSize := 50
	if p := r.GinCtx.Query("page"); p != "" {
		if v, err := strconv.Atoi(p); err == nil && v > 0 {
			page = v
		}
	}
	if ps := r.GinCtx.Query("page_size"); ps != "" {
		if v, err := strconv.Atoi(ps); err == nil && v > 0 && v <= 200 {
			pageSize = v
		}
	}
	payments, total, err := r.State.PaymentService.GetPaginatedByUserID(r.Request.Context(), path.UserID, page, pageSize)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}
	data := make([]api.PaymentObject, len(payments))
	for i, p := range payments {
		data[i] = PaymentToAPI(p, nil)
	}
	offset := (page - 1) * pageSize
	hasMore := offset+len(data) < total
	r.GinCtx.JSON(http.StatusOK, map[string]interface{}{"object": "list", "data": data, "total": total, "limit": pageSize, "offset": offset, "has_more": hasMore})
}

func AdminCreateOffChannelPayment(r *httprequest.Request) {
	var path adminOffChannelPaymentPath
	if err := r.Inner().ShouldBindUri(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	var req adminOffChannelPaymentRequest
	if !r.BindJSON(&req) {
		return
	}
	priceID, err := api.ParsePriceID(strings.TrimSpace(req.PriceID))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid price_id")
		return
	}
	transactionID := strings.TrimSpace(req.TransactionID)
	if transactionID == "" {
		r.ErrorJSON(http.StatusBadRequest, "transaction_id is required")
		return
	}
	if req.Amount != nil && *req.Amount < 0 {
		r.ErrorJSON(http.StatusBadRequest, "amount must be >= 0")
		return
	}
	var purchasedAt *time.Time
	if strings.TrimSpace(req.PurchasedAt) != "" {
		tm, err := time.Parse(time.RFC3339, strings.TrimSpace(req.PurchasedAt))
		if err != nil {
			r.ErrorJSON(http.StatusBadRequest, "purchased_at must be RFC3339")
			return
		}
		tm = tm.UTC()
		purchasedAt = &tm
	}
	if existing, err := r.State.PaymentService.GetByTransactionID(r.Request.Context(), models.ProcessorManual, transactionID); err == nil {
		r.Inner().JSON(http.StatusOK, map[string]any{"payment_id": existing.ID.String(), "status": "exists"})
		return
	}
	amount := int64(0)
	if req.Amount != nil {
		amount = *req.Amount
	}
	result, err := r.State.CheckoutService.RegisterPurchase(r.Request.Context(), &payments.RegisterPurchaseRequest{UserID: path.UserID, PriceID: priceID, Processor: string(models.ProcessorManual), TransactionID: transactionID, Amount: amount, Currency: strings.TrimSpace(req.Currency), PurchasedAt: purchasedAt, DiscountCode: req.DiscountCode, DiscountReason: req.DiscountReason, DiscountMetadata: req.DiscountMetadata})
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.Inner().JSON(http.StatusCreated, map[string]any{"payment_id": result.PaymentID.String(), "entitlements": result.Entitlements, "delayed_start": result.DelayedStart, "eligibility": result.Eligibility})
}
