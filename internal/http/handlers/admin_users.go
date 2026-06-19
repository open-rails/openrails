package handlers

import (
	"errors"
	"net/http"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/modules/vault"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/query"
	log "github.com/sirupsen/logrus"
)

type adminUserPath struct {
	UserID string `uri:"user_id" binding:"required"`
}

// adminUserBillingProfile is the composite admin user-detail (#528): one read
// returns the user's billing sections so admins don't fan out across dedicated
// per-section endpoints.
type adminUserBillingProfile struct {
	CustomerID     string                       `json:"customer_id"`
	Subscriptions  []models.Subscription        `json:"subscriptions"`
	Entitlements   []models.Entitlement         `json:"entitlements"`
	Payments       []*models.Payment            `json:"payments"`
	PaymentMethods []paymentMethodResponse      `json:"payment_methods"`
	CreditBalance  []adminCreditBalanceResponse `json:"credit_balance"`
	ProductAccess  []models.ProductAccessGrant  `json:"product_access"`
}

type adminCreditBalanceResponse struct {
	Currency              string `json:"currency"`
	DisplayName           string `json:"display_name"`
	Unit                  string `json:"unit"`
	DecimalPlaces         int    `json:"decimal_places"`
	Balance               int64  `json:"balance"`
	HeldBalance           int64  `json:"held_balance"`
	OutstandingOwedAmount int64  `json:"outstanding_owed_amount"`
}

type adminPaymentMethodPath struct {
	UserID string `uri:"user_id" binding:"required"`
	ID     string `uri:"id" binding:"required"`
}

type adminSubscriptionPath struct {
	SubscriptionID string `uri:"id" binding:"required"`
}

type adminCancelSubscriptionRequest struct {
	Reason string `json:"reason"`
}

func GetAdminUserBillingProfile(r *httprequest.Request) {
	var path adminUserPath
	if err := r.ShouldBindURI(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Request.Context()
	now := time.Now()
	profile := adminUserBillingProfile{
		CustomerID:     identity.CustomerIDFromString(path.UserID).UUID().String(),
		Subscriptions:  []models.Subscription{},
		Entitlements:   []models.Entitlement{},
		Payments:       []*models.Payment{},
		PaymentMethods: []paymentMethodResponse{},
		CreditBalance:  []adminCreditBalanceResponse{},
		ProductAccess:  []models.ProductAccessGrant{},
	}
	if r.State.SubscriptionService != nil {
		subs, err := r.State.SubscriptionService.GetActiveSubscriptionsByUserID(ctx, path.UserID)
		if err == nil && len(subs) > 0 {
			profile.Subscriptions = subs
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
	if r.State.PaymentMethodService != nil {
		if pms, err := r.State.PaymentMethodService.GetByUserID(ctx, path.UserID); err == nil && len(pms) > 0 {
			profile.PaymentMethods = paymentMethodsToAPI(pms)
		}
	}
	if r.State.MoneyService != nil {
		payer := identity.CustomerIDFromString(path.UserID)
		if !payer.IsZero() {
			balances, err := r.State.MoneyService.ListBalancesForCustomer(ctx, payer)
			if err != nil {
				log.WithError(err).WithField("user_id", path.UserID).Error("failed to load credit balances")
				r.ErrorJSON(http.StatusInternalServerError, "failed to load credit balances")
				return
			}
			for _, bal := range balances {
				owed, err := r.State.MoneyService.GetOutstandingOwed(ctx, payer, bal.Currency)
				if err != nil {
					r.ErrorJSON(http.StatusInternalServerError, "failed to load outstanding owed amount")
					return
				}
				profile.CreditBalance = append(profile.CreditBalance, adminCreditBalanceResponse{
					Currency:              bal.Currency,
					DisplayName:           bal.Currency,
					Unit:                  bal.Currency,
					DecimalPlaces:         currencyDecimals(bal.Currency),
					Balance:               bal.Balance,
					HeldBalance:           bal.HeldBalance,
					OutstandingOwedAmount: owed,
				})
			}
		}
	}
	if svc := productAccessService(r); svc != nil {
		if grants, err := svc.ListAllGrantsByUser(ctx, path.UserID); err == nil && len(grants) > 0 {
			profile.ProductAccess = grants
		}
	}
	r.SuccessJSON(profile)
}

func DeleteAdminUserPaymentMethod(r *httprequest.Request) {
	var path adminPaymentMethodPath
	if err := r.ShouldBindURI(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	id, err := api.ParsePaymentMethodID(path.ID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid payment method ID")
		return
	}
	if r.State.PaymentMethodService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "payment method service unavailable")
		return
	}
	paymentMethod, err := r.State.PaymentMethodService.ValidatePaymentMethodOperation(r.Request.Context(), id, path.UserID)
	if err != nil {
		switch {
		case errors.Is(err, vault.ErrPaymentMethodNotFound):
			r.ErrorJSON(http.StatusNotFound, "payment method not found")
		case errors.Is(err, vault.ErrPaymentMethodAccessDenied):
			r.ErrorJSON(http.StatusForbidden, "payment method does not belong to user")
		default:
			log.WithError(err).WithFields(log.Fields{"payment_method_id": id, "user_id": path.UserID}).Error("failed to validate admin payment method delete")
			r.ErrorJSON(http.StatusInternalServerError, "failed to validate payment method")
		}
		return
	}
	for _, sub := range paymentMethod.Subscriptions {
		if sub.Status == "active" || sub.Status == "pending" || sub.Status == "past_due" {
			r.ErrorJSON(http.StatusConflict, "cannot delete payment method linked to active, pending, or past_due subscription")
			return
		}
	}
	if err := r.State.PaymentMethodService.Delete(r.Request.Context(), id); err != nil {
		if errors.Is(err, vault.ErrPaymentMethodNotFound) {
			r.ErrorJSON(http.StatusNotFound, "payment method not found")
			return
		}
		log.WithError(err).WithFields(log.Fields{"payment_method_id": id, "user_id": path.UserID}).Error("failed to delete admin payment method")
		r.ErrorJSON(http.StatusInternalServerError, "failed to delete payment method")
		return
	}
	r.SuccessJSON(map[string]any{"success": true})
}

func GetAdminSubscriptions(r *httprequest.Request) {
	queryOpts := query.QueryOptions[subscriptions.GetSubscriptionsFilters]{Limit: 50, Offset: 0}
	if err := r.ShouldBindQuery(&queryOpts); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	var filters subscriptions.GetSubscriptionsFilters
	if err := r.ShouldBindQuery(&filters); err != nil {
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
	if err := r.ShouldBindURI(&path); err != nil {
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

func AdminCancelSubscription(r *httprequest.Request) {
	subscriptionID, err := api.ParseSubscriptionID(r.Param("id"))
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
