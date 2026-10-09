package handlers

import (
	"github.com/open-rails/openrails/billing"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/pagination"
	log "github.com/sirupsen/logrus"
)

type adminUserPath struct {
	UserID string `uri:"customer_id" binding:"required"`
}

// adminProfileSubscriptionWindow bounds the profile's subscriptions section to
// the customer's most recent non-deleted subscriptions of any status.
const adminProfileSubscriptionWindow = 100

type adminSubscriptionPath struct {
	SubscriptionID string `uri:"id" binding:"required"`
}

// GetCustomerBillingProfile is one customer's billing at a glance: each
// section is the shape its own route serves.
func GetCustomerBillingProfile(r *httprequest.Request) {
	customerID, ok := customerParam(r)
	if !ok {
		return
	}
	svc, ok := billingService(r)
	if !ok {
		return
	}
	ctx := r.Request.Context()
	customer, err := svc.GetCustomer(ctx, customerID)
	if err != nil {
		writeRefusal(r, err, "customer read failed")
		return
	}
	now := r.Clock.Now()
	subject := customerID.String()
	profile := billing.CustomerBillingProfile{
		Customer:       *customer,
		Balances:       []billing.Balance{},
		Subscriptions:  []billing.Subscription{},
		Entitlements:   billing.ListPage[billing.CustomerEntitlement]{Items: []billing.CustomerEntitlement{}},
		Payments:       []billing.Payment{},
		PaymentMethods: []billing.PaymentMethod{},
		ProductAccess:  billing.ListPage[billing.ProductAccessGrant]{Items: []billing.ProductAccessGrant{}},
	}
	if r.State.MoneyService != nil {
		balances, err := r.State.MoneyService.ListBalancesForCustomer(ctx, customerID)
		if err != nil {
			r.InternalError("failed to load balances", err)
			return
		}
		for _, bal := range balances {
			balance, err := svc.GetBalance(ctx, customerID, bal.Currency)
			if err != nil {
				r.InternalError("failed to load balance", err)
				return
			}
			profile.Balances = append(profile.Balances, *balance)
		}
	}
	if r.State.SubscriptionService != nil {
		// The customer's most recent subscriptions of any status.
		subs, _, err := r.State.SubscriptionService.GetPaginatedByUserID(ctx, subject, 1, adminProfileSubscriptionWindow)
		if err == nil {
			for i := range subs {
				sub := &subs[i]
				profile.Subscriptions = append(profile.Subscriptions, subscriptionView(&subscriptions.AdminSubscriptionResponse{Subscription: sub, Price: sub.Price}, now))
			}
		}
	}
	if r.State.EntitlementService != nil {
		keys, more, err := r.State.EntitlementService.ListEntitlementsPage(ctx, customerID.UUID(), "", "", billing.DefaultPageLimit, now)
		if err != nil {
			r.InternalError("failed to load entitlements", err)
			return
		}
		for _, key := range keys {
			profile.Entitlements.Items = append(profile.Entitlements.Items, billing.CustomerEntitlement{Entitlement: key})
		}
		if more {
			profile.Entitlements.Next = pagination.Encode(keys[len(keys)-1])
		}
	}
	if r.State.PaymentService != nil {
		payments, err := r.State.PaymentService.GetByUserID(ctx, subject)
		if err == nil {
			for _, p := range payments {
				profile.Payments = append(profile.Payments, PaymentToAPI(p, nil))
			}
		}
	}
	if r.State.PaymentMethodService != nil {
		if pms, err := r.State.PaymentMethodService.GetByUserID(ctx, subject); err == nil && len(pms) > 0 {
			// The cards are a section of the profile, not its point: a read
			// failure leaves the section empty rather than failing the profile.
			if methods, err := paymentMethodsView(r, customerID, pms); err == nil {
				profile.PaymentMethods = methods
			} else {
				log.WithError(err).WithField("customer_id", subject).Warn("failed to read payment methods; profile returned without them")
			}
		}
	}
	if svc := productAccessService(r); svc != nil {
		rows, more, err := svc.ListPage(ctx, customerID.UUID(), nil, billing.DefaultPageLimit, false)
		if err != nil {
			r.InternalError("failed to load product access", err)
			return
		}
		for _, row := range rows {
			profile.ProductAccess.Items = append(profile.ProductAccess.Items, productAccessGrant(row, now))
		}
		if more {
			profile.ProductAccess.Next = pagination.Encode(rows[len(rows)-1].ID)
		}
	}
	r.SuccessJSON(profile)
}

// GetAdminSubscriptions is one page of the merchant's subscriptions,
// newest first.
func GetAdminSubscriptions(r *httprequest.Request) {
	var filters subscriptions.GetSubscriptionsFilters
	if !r.BindQuery(&filters) {
		return
	}
	page, ok := r.Page()
	if !ok {
		return
	}
	svc := r.State.AdminSubscriptionService
	if svc == nil {
		r.ErrorCode(billing.CodeInternalError, "admin subscription service unavailable")
		return
	}
	subs, err := svc.ListSubscriptions(r.Request.Context(), filters, page)
	if err != nil {
		writeRefusal(r, err, "failed to list subscriptions")
		return
	}
	now := r.Clock.Now()
	r.SuccessJSON(pagination.Map(subs, func(sub *subscriptions.AdminSubscriptionResponse) billing.Subscription {
		return subscriptionView(sub, now)
	}))
}

func GetAdminSubscription(r *httprequest.Request) {
	var path adminSubscriptionPath
	if err := r.ShouldBindURI(&path); err != nil {
		r.ErrorCode(billing.CodeInvalidParam, err.Error())
		return
	}
	typedSubscriptionID, err := billing.ParseSubscriptionID(path.SubscriptionID)
	if err != nil || typedSubscriptionID.IsZero() {
		r.ErrorCode(billing.CodeInvalidParam, "invalid subscription ID")
		return
	}
	subscriptionID := typedSubscriptionID.UUID()
	svc := r.State.AdminSubscriptionService
	if svc == nil {
		r.ErrorCode(billing.CodeInternalError, "admin subscription service unavailable")
		return
	}
	subscription, err := svc.GetSubscriptionByID(r.Request.Context(), subscriptionID)
	if err != nil {
		writeRefusal(r, err, "failed to load subscription")
		return
	}
	r.SuccessJSON(subscriptionView(subscription, r.Clock.Now()))
}
