package handlers

import (
	"net/http"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

type AdminCCBillPayment struct {
	SubscriptionID string  `json:"subscription_id,omitempty"`
	TransactionID  string  `json:"transaction_id,omitempty"`
	Status         string  `json:"status,omitempty"`
	StartDate      string  `json:"start_date,omitempty"`
	ExpiryDate     string  `json:"expiry_date,omitempty"`
	ManualExpiry   *string `json:"manual_expiry,omitempty"`
}

type AdminCCBillMetrics struct {
	Successful int `json:"successful"`
	Failed     int `json:"failed"`
}

// GetAdminUserCCBill returns basic CCBill payment details for a user (latest payment)
// GET /v1/admin/users/:user_id/ccbill
func GetAdminUserCCBill(r *httprequest.Request) {
	var path AdminUserPath
	if err := r.Inner().ShouldBindUri(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}

	if r.State.PaymentService == nil || r.State.SubscriptionService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "payment services unavailable")
		return
	}

	subscription, err := r.State.SubscriptionService.GetActiveSubscription(r.Request.Context(), path.UserID)
	if err != nil || subscription == nil {
		r.ErrorJSON(http.StatusNotFound, "active subscription not found")
		return
	}
	if subscription.Processor != models.ProcessorCCBill {
		r.ErrorJSON(http.StatusNotFound, "ccbill subscription not found")
		return
	}

	payment, err := r.State.PaymentService.GetLatestBySubscriptionID(r.Request.Context(), subscription.ID)
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, "ccbill payment not found")
		return
	}

	resp := AdminCCBillPayment{
		SubscriptionID: subscription.ProcessorSubscriptionID,
		TransactionID:  payment.TransactionID,
		Status:         string(subscription.Status),
		StartDate:      subscription.StartedAt.Format(time.RFC3339),
		ExpiryDate: func() string {
			if subscription.CurrentPeriodEndsAt != nil {
				return subscription.CurrentPeriodEndsAt.Format(time.RFC3339)
			}
			return ""
		}(),
	}

	r.SuccessJSON(resp)
}

// GetAdminUserCCBillMetrics returns rebill metrics for CCBill payments
// GET /v1/admin/users/:user_id/ccbill/metrics
func GetAdminUserCCBillMetrics(r *httprequest.Request) {
	var path AdminUserPath
	if err := r.Inner().ShouldBindUri(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}

	if r.State.PaymentService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "payment service unavailable")
		return
	}

	success, failed, err := r.State.PaymentService.CountByUserAndProcessor(r.Request.Context(), path.UserID, models.ProcessorCCBill)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}

	r.SuccessJSON(AdminCCBillMetrics{
		Successful: success,
		Failed:     failed,
	})
}
