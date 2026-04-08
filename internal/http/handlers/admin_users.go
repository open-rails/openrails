package handlers

import (
	"net/http"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/payments/processors"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/query"
)

type adminUserPath struct {
	UserID string `uri:"user_id" binding:"required"`
}

type adminUserBillingProfile struct {
	UserID       string               `json:"user_id"`
	Subscription *models.Subscription `json:"subscription,omitempty"`
	Entitlements []models.Entitlement `json:"entitlements"`
	Payments     []*models.Payment    `json:"payments"`
}

type adminSubscriptionPath struct {
	SubscriptionID string `uri:"id" binding:"required"`
}

type adminNMIPayment struct {
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

type adminNMIMetrics struct {
	Successful int `json:"successful"`
	Failed     int `json:"failed"`
}

type adminCCBillPayment struct {
	SubscriptionID string  `json:"subscription_id,omitempty"`
	TransactionID  string  `json:"transaction_id,omitempty"`
	Status         string  `json:"status,omitempty"`
	StartDate      string  `json:"start_date,omitempty"`
	ExpiryDate     string  `json:"expiry_date,omitempty"`
	ManualExpiry   *string `json:"manual_expiry,omitempty"`
}

type adminCCBillMetrics struct {
	Successful int `json:"successful"`
	Failed     int `json:"failed"`
}

type adminCancelSubscriptionRequest struct {
	Reason string `json:"reason"`
}

func GetAdminUserBillingProfile(r *httprequest.Request) {
	var path adminUserPath
	if err := r.Inner().ShouldBindUri(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Request.Context()
	now := time.Now()
	profile := adminUserBillingProfile{UserID: path.UserID, Entitlements: []models.Entitlement{}, Payments: []*models.Payment{}}
	if r.State.SubscriptionService != nil {
		sub, err := r.State.SubscriptionService.GetActiveSubscription(ctx, path.UserID)
		if err == nil {
			profile.Subscription = sub
		}
	}
	if r.State.EntitlementService != nil {
		ents, err := r.State.EntitlementService.ListActiveRecords(ctx, path.UserID, now)
		if err == nil && len(ents) > 0 {
			profile.Entitlements = ents
		}
	}
	if r.State.PaymentService != nil {
		payments, err := r.State.PaymentService.GetByUserID(ctx, path.UserID)
		if err == nil && len(payments) > 0 {
			profile.Payments = payments
		}
	}
	r.SuccessJSON(profile)
}

func GetAdminSubscriptions(r *httprequest.Request) {
	queryOpts := query.QueryOptions[subscriptions.GetSubscriptionsFilters]{Limit: 50, Offset: 0}
	if err := r.Inner().ShouldBindQuery(&queryOpts); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	var filters subscriptions.GetSubscriptionsFilters
	if err := r.Inner().ShouldBindQuery(&filters); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	queryOpts.Filters = filters
	svc := r.State.AdminSubscriptionService
	if svc == nil {
		r.ErrorJSON(http.StatusInternalServerError, "admin subscription service unavailable")
		return
	}
	subscriptions, total, err := svc.GetAllSubscriptions(r.Request.Context(), &queryOpts)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}
	r.SuccessJSONPaginated(subscriptions, total, queryOpts.Limit, queryOpts.Offset)
}

func GetAdminSubscription(r *httprequest.Request) {
	var path adminSubscriptionPath
	if err := r.Inner().ShouldBindUri(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	subscriptionID, err := api.ParseSubscriptionID(path.SubscriptionID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid subscription ID")
		return
	}
	svc := r.State.AdminSubscriptionService
	if svc == nil {
		r.ErrorJSON(http.StatusInternalServerError, "admin subscription service unavailable")
		return
	}
	subscription, err := svc.GetSubscriptionByID(r.Request.Context(), subscriptionID)
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, err.Error())
		return
	}
	r.SuccessJSON(subscription)
}

func GetAdminUserNMI(r *httprequest.Request) {
	var path adminUserPath
	if err := r.Inner().ShouldBindUri(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	if r.State.PaymentService == nil || r.State.PaymentMethodService == nil || r.State.SubscriptionService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "payment services unavailable")
		return
	}
	subscription, err := r.State.SubscriptionService.GetActiveSubscription(r.Request.Context(), path.UserID)
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, "active subscription not found")
		return
	}
	if !processors.IsNMIBackedProcessor(subscription.Processor) {
		r.ErrorJSON(http.StatusNotFound, "nmi-backed subscription not found")
		return
	}
	payment, err := r.State.PaymentService.GetLatestBySubscriptionID(r.Request.Context(), subscription.ID)
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, "nmi-backed payment not found")
		return
	}
	var pmVault string
	if subscription.PaymentMethodID != nil {
		if pm, err := r.State.PaymentMethodService.GetByID(r.Request.Context(), *subscription.PaymentMethodID); err == nil {
			pmVault = pm.VaultID
		}
	}
	resp := adminNMIPayment{VaultID: pmVault, OrderID: subscription.ID.String(), Amount: payment.Amount, Currency: payment.Currency, TransactionID: payment.TransactionID, Status: string(subscription.Status), StartDate: subscription.StartedAt.Format(time.RFC3339), ExpiryDate: func() string {
		if subscription.CurrentPeriodEndsAt != nil {
			return subscription.CurrentPeriodEndsAt.Format(time.RFC3339)
		}
		return ""
	}(), TotalSoFar: payment.Amount}
	r.SuccessJSON(resp)
}

func GetAdminUserNMIMetrics(r *httprequest.Request) {
	var path adminUserPath
	if err := r.Inner().ShouldBindUri(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	if r.State.PaymentService == nil || r.State.SubscriptionService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "payment services unavailable")
		return
	}
	subscription, err := r.State.SubscriptionService.GetActiveSubscription(r.Request.Context(), path.UserID)
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, "active subscription not found")
		return
	}
	if !processors.IsNMIBackedProcessor(subscription.Processor) {
		r.ErrorJSON(http.StatusNotFound, "nmi-backed subscription not found")
		return
	}
	success, failed, err := r.State.PaymentService.CountByUserAndProcessor(r.Request.Context(), path.UserID, subscription.Processor)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}
	r.SuccessJSON(adminNMIMetrics{Successful: success, Failed: failed})
}

func GetAdminUserCCBill(r *httprequest.Request) {
	var path adminUserPath
	if err := r.Inner().ShouldBindUri(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	if r.State.PaymentService == nil || r.State.SubscriptionService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "payment services unavailable")
		return
	}
	subscription, err := r.State.SubscriptionService.GetActiveSubscription(r.Request.Context(), path.UserID)
	if err != nil {
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
	resp := adminCCBillPayment{SubscriptionID: subscription.ProcessorSubscriptionID, TransactionID: payment.TransactionID, Status: string(subscription.Status), StartDate: subscription.StartedAt.Format(time.RFC3339), ExpiryDate: func() string {
		if subscription.CurrentPeriodEndsAt != nil {
			return subscription.CurrentPeriodEndsAt.Format(time.RFC3339)
		}
		return ""
	}()}
	r.SuccessJSON(resp)
}

func GetAdminUserCCBillMetrics(r *httprequest.Request) {
	var path adminUserPath
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
	r.SuccessJSON(adminCCBillMetrics{Successful: success, Failed: failed})
}

func AdminCancelSubscription(r *httprequest.Request) {
	subscriptionID, err := api.ParseSubscriptionID(r.GinCtx.Param("id"))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid subscription ID")
		return
	}
	req := new(adminCancelSubscriptionRequest)
	if !r.BindJSON(req) {
		r.ErrorJSON(http.StatusBadRequest, "invalid request body")
		return
	}
	if err := r.State.AdminSubscriptionService.CancelSubscription(r.Request.Context(), subscriptionID, req.Reason); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
		return
	}
	r.SuccessJSONMessage("subscription cancelled successfully")
}
