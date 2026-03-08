package handlers

import (
	"net/http"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

type AdminMobiusPayment struct {
	VaultID       string  `json:"vault_id,omitempty"`
	OrderID       string  `json:"order_id,omitempty"`
	Amount        int64   `json:"amount"`
	Currency      string  `json:"currency,omitempty"`
	TransactionID string  `json:"transaction_id,omitempty"`
	Status        string  `json:"status,omitempty"`
	StartDate     string  `json:"start_date,omitempty"`
	ExpiryDate    string  `json:"expiry_date,omitempty"`
	TotalSoFar    int64   `json:"total_so_far,omitempty"`
	ManualExpiry  *string `json:"manual_expiry,omitempty"`
}

type AdminMobiusMetrics struct {
	Successful int `json:"successful"`
	Failed     int `json:"failed"`
}

// GetAdminUserMobius returns basic Mobius payment details for a user (latest payment)
// GET /v1/admin/users/:user_id/mobius
func GetAdminUserMobius(r *httprequest.Request) {
	var path AdminUserPath
	if err := r.Inner().ShouldBindUri(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}

	if r.State.PaymentService == nil || r.State.PaymentMethodService == nil || r.State.SubscriptionService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "payment services unavailable")
		return
	}

	subscription, err := r.State.SubscriptionService.GetActiveSubscription(r.Request.Context(), path.UserID)
	if err != nil || subscription == nil {
		r.ErrorJSON(http.StatusNotFound, "active subscription not found")
		return
	}
	if subscription.Processor != models.ProcessorMobius {
		r.ErrorJSON(http.StatusNotFound, "mobius subscription not found")
		return
	}

	payment, err := r.State.PaymentService.GetLatestBySubscriptionID(r.Request.Context(), subscription.ID)
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, "mobius payment not found")
		return
	}

	var pmVault string
	if subscription.PaymentMethodID != nil {
		if pm, err := r.State.PaymentMethodService.GetByID(r.Request.Context(), *subscription.PaymentMethodID); err == nil && pm != nil {
			pmVault = pm.VaultID
		}
	}

	resp := AdminMobiusPayment{
		VaultID:       pmVault,
		OrderID:       subscription.ID.String(),
		Amount:        payment.Amount,
		Currency:      payment.Currency,
		TransactionID: payment.TransactionID,
		Status:        string(subscription.Status),
		StartDate:     subscription.StartedAt.Format(time.RFC3339),
		ExpiryDate: func() string {
			if subscription.CurrentPeriodEndsAt != nil {
				return subscription.CurrentPeriodEndsAt.Format(time.RFC3339)
			}
			return ""
		}(),
		TotalSoFar: payment.Amount,
	}

	r.SuccessJSON(resp)
}

// GetAdminUserMobiusMetrics returns rebill metrics for Mobius payments
// GET /v1/admin/users/:user_id/mobius/metrics
func GetAdminUserMobiusMetrics(r *httprequest.Request) {
	var path AdminUserPath
	if err := r.Inner().ShouldBindUri(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}

	if r.State.PaymentService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "payment service unavailable")
		return
	}

	success, failed, err := r.State.PaymentService.CountByUserAndProcessor(r.Request.Context(), path.UserID, models.ProcessorMobius)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}

	r.SuccessJSON(AdminMobiusMetrics{
		Successful: success,
		Failed:     failed,
	})
}
