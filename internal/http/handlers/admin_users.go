package handlers

import (
	"net/http"
	"strconv"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/query"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/riverqueue/river"
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

type AdminCancelSubscriptionRequest = billing.CancelSubscriptionRequest

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
		Entitlements:   []billing.EntitlementRecord{},
		Payments:       []billing.Payment{},
		PaymentMethods: []billing.PaymentMethod{},
		ProductAccess:  []billing.ProductAccessGrant{},
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
		ents, err := r.State.EntitlementService.ListActiveRecords(ctx, subject, now)
		if err == nil {
			for i := range ents {
				profile.Entitlements = append(profile.Entitlements, entitlementRecordFromModel(&ents[i]))
			}
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
		if grants, err := svc.ListAllGrantsByUser(ctx, subject); err == nil && len(grants) > 0 {
			profile.ProductAccess = productAccessResponses(r, grants)
		}
	}
	r.SuccessJSON(profile)
}

func GetAdminSubscriptions(r *httprequest.Request) {
	limit, offset, ok := offsetPage(r)
	if !ok {
		return
	}
	queryOpts := query.QueryOptions[subscriptions.GetSubscriptionsFilters]{Limit: limit, Offset: offset}
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
	out := make([]billing.Subscription, 0, len(subscriptions))
	for _, sub := range subscriptions {
		out = append(out, subscriptionView(sub, r.Clock.Now()))
	}
	r.SuccessJSON(api.NewList(out, total, limit, offset))
}

func GetAdminSubscription(r *httprequest.Request) {
	var path adminSubscriptionPath
	if err := r.ShouldBindURI(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	typedSubscriptionID, err := billing.ParseSubscriptionID(path.SubscriptionID)
	if err != nil || typedSubscriptionID.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid subscription ID")
		return
	}
	subscriptionID := typedSubscriptionID.UUID()
	svc := r.State.AdminSubscriptionService
	if svc == nil {
		r.ErrorJSON(http.StatusInternalServerError, "admin subscription service unavailable")
		return
	}
	subscription, err := svc.GetSubscriptionByID(r.Request.Context(), subscriptionID)
	if err != nil {
		writeRefusal(r, err, "failed to load subscription")
		return
	}
	r.SuccessJSON(subscriptionView(subscription, r.Clock.Now()))
}

func AdminCancelSubscription(r *httprequest.Request) {
	typedSubscriptionID, err := billing.ParseSubscriptionID(r.Param("id"))
	if err != nil || typedSubscriptionID.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid subscription ID")
		return
	}
	subscriptionID := typedSubscriptionID.UUID()
	req := new(AdminCancelSubscriptionRequest)
	if !r.BindJSON(req) {
		r.ErrorJSON(http.StatusBadRequest, "invalid request body")
		return
	}
	if err := r.State.AdminSubscriptionService.CancelSubscription(r.Request.Context(), subscriptionID, req.Reason, req.RevokeAccess, req.AccountDeletion); err != nil {
		writeRefusal(r, err, "failed to cancel subscription")
		return
	}
	r.SuccessJSONMessage("subscription cancelled successfully")
}

func AdminResumeSubscription(r *httprequest.Request) {
	typedSubscriptionID, err := billing.ParseSubscriptionID(r.Param("id"))
	if err != nil || typedSubscriptionID.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid subscription ID")
		return
	}
	subscriptionID := typedSubscriptionID.UUID()
	if r.State.SubscriptionService == nil || r.State.RiverProducer == nil {
		r.ErrorJSON(http.StatusInternalServerError, "subscription service unavailable")
		return
	}
	sub, err := r.State.SubscriptionService.GetByID(r.Request.Context(), subscriptionID)
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, "subscription not found")
		return
	}
	now := r.Clock.Now().UTC()
	if !subscriptions.Resumable(sub, now) {
		r.ErrorJSON(http.StatusBadRequest, "subscription is not resumable")
		return
	}
	if _, err := r.State.RiverProducer.Insert(r.Request.Context(), riverjobs.ResumeSubscriptionArgs{
		MerchantID:     sub.MerchantID,
		UserID:         sub.CustomerID.String(),
		SubscriptionID: subscriptionID,
	}, &river.InsertOpts{
		Queue:      riverjobs.QueueBilling,
		UniqueOpts: subscriptionLifecycleUniqueOpts(),
	}); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to enqueue resume")
		return
	}
	r.JSON(http.StatusAccepted, map[string]any{"status": "queued"})
}

// offsetPage reads an offset page: limit 1-100 (default 50) and offset.
func offsetPage(r *httprequest.Request) (int, int, bool) {
	limit, offset := 50, 0
	for name, target := range map[string]*int{"limit": &limit, "offset": &offset} {
		if raw := r.Query(name); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 {
				r.APIError(api.Coded(billing.CodeInvalidQuery, "invalid "+name).WithParam(name))
				return 0, 0, false
			}
			*target = n
		}
	}
	if limit < 1 || limit > 100 {
		r.APIError(api.Coded(billing.CodeInvalidQuery, "limit must be between 1 and 100").WithParam("limit"))
		return 0, 0, false
	}
	return limit, offset, true
}
