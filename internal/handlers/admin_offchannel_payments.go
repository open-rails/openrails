package handlers

import (
	"net/http"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/services"
	"github.com/open-rails/openrails/pkg/api"
)

type AdminOffChannelPaymentPath struct {
	UserID string `uri:"user_id" binding:"required"`
}

type AdminOffChannelPaymentRequest struct {
	PriceID          string         `json:"price_id" binding:"required"`
	TransactionID    string         `json:"transaction_id" binding:"required"`
	Amount           *int64         `json:"amount,omitempty"`
	Currency         string         `json:"currency,omitempty"`
	PurchasedAt      string         `json:"purchased_at,omitempty"` // RFC3339
	DiscountCode     *string        `json:"discount_code,omitempty"`
	DiscountReason   *string        `json:"discount_reason,omitempty"`
	DiscountMetadata map[string]any `json:"discount_metadata,omitempty"`
}

// AdminCreateOffChannelPayment records an off-channel/manual purchase (cash/bank transfer/etc)
// and grants entitlements/credits sourced from the resulting Payment.
//
// POST /v1/admin/users/:user_id/payments/off-channel
func AdminCreateOffChannelPayment(r *httprequest.Request) {
	var path AdminOffChannelPaymentPath
	if err := r.Inner().ShouldBindUri(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}

	var req AdminOffChannelPaymentRequest
	if !r.BindJSON(&req) {
		return
	}

	if r.State == nil || r.State.CheckoutService == nil || r.State.PaymentService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing services unavailable")
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

	// Idempotency: if this (processor, transaction_id) already exists, return it.
	if existing, err := r.State.PaymentService.GetByTransactionID(r.Request.Context(), models.ProcessorManual, transactionID); err == nil && existing != nil {
		r.Inner().JSON(http.StatusOK, map[string]any{
			"payment_id": existing.ID.String(),
			"status":     "exists",
		})
		return
	}

	amount := int64(0)
	if req.Amount != nil {
		amount = *req.Amount
	}

	result, err := r.State.CheckoutService.RegisterPurchase(r.Request.Context(), &services.RegisterPurchaseRequest{
		UserID:           path.UserID,
		PriceID:          priceID,
		Processor:        string(models.ProcessorManual),
		TransactionID:    transactionID,
		Amount:           amount,
		Currency:         strings.TrimSpace(req.Currency),
		PurchasedAt:      purchasedAt,
		DiscountCode:     req.DiscountCode,
		DiscountReason:   req.DiscountReason,
		DiscountMetadata: req.DiscountMetadata,
	})
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}

	r.Inner().JSON(http.StatusCreated, map[string]any{
		"payment_id":    result.PaymentID.String(),
		"entitlements":  result.Entitlements,
		"delayed_start": result.DelayedStart,
		"eligibility":   result.Eligibility,
	})
}
