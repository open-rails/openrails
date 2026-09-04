package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/open-rails/openrails/pkg/query"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

type adminUserPath struct {
	UserID string `uri:"customer_id" binding:"required"`
}

// adminUserBillingProfile is the composite admin user-detail (#528): one read
// returns the user's billing sections so admins don't fan out across dedicated
// per-section endpoints.
type adminUserBillingProfile struct {
	CustomerID     string                       `json:"customer_id"`
	Email          *string                      `json:"email,omitempty"`
	TrustLevel     string                       `json:"trust_level,omitempty"`
	Subscriptions  []models.Subscription        `json:"subscriptions"`
	Entitlements   []models.Entitlement         `json:"entitlements"`
	Payments       []*models.Payment            `json:"payments"`
	PaymentMethods []paymentMethodResponse      `json:"payment_methods"`
	CreditBalance  []adminCreditBalanceResponse `json:"credit_balance"`
	ProductAccess  []models.ProductAccessGrant  `json:"product_access"`
}

type adminCreditBalanceResponse struct {
	Currency              string `json:"currency"`
	TrustLevel            string `json:"trust_level,omitempty"`
	DisplayName           string `json:"display_name"`
	Unit                  string `json:"unit"`
	DecimalPlaces         int    `json:"decimal_places"`
	Balance               int64  `json:"balance"`
	HeldBalance           int64  `json:"held_balance"`
	OutstandingOwedAmount int64  `json:"outstanding_owed_amount"`
}

type adminSubscriptionPath struct {
	SubscriptionID string `uri:"id" binding:"required"`
}

type adminCancelSubscriptionRequest struct {
	Reason       string `json:"reason"`
	RevokeAccess bool   `json:"revoke_access,omitempty"`
}

func GetAdminUserBillingProfile(r *httprequest.Request) {
	var path adminUserPath
	if err := r.ShouldBindURI(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	customerID := identity.CustomerIDFromString(path.UserID)
	if customerID.UUID() == uuid.Nil {
		// CustomerIDFromString coerces empty/non-UUID input to the zero id, which
		// this caller must reject (#784): otherwise a malformed customer_id path
		// segment (e.g. "does-not-exist") returns a 200 empty profile coerced to
		// the zero UUID instead of a 400. A well-formed but never-seen id is still
		// a valid (empty) profile by design — customers are implicit.
		r.ErrorJSON(http.StatusBadRequest, "invalid customer id")
		return
	}
	ctx := r.Request.Context()
	now := r.Clock.Now()
	profile := adminUserBillingProfile{
		CustomerID:     customerID.UUID().String(),
		Subscriptions:  []models.Subscription{},
		Entitlements:   []models.Entitlement{},
		Payments:       []*models.Payment{},
		PaymentMethods: []paymentMethodResponse{},
		CreditBalance:  []adminCreditBalanceResponse{},
		ProductAccess:  []models.ProductAccessGrant{},
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "merchant scope required")
		return
	}
	email, err := r.State.DB.Gen(ctx).GetLatestCustomerEmail(ctx, gen.GetLatestCustomerEmailParams{
		CustomerID: customerID.UUID(),
		MerchantID: merchantID.UUID(),
	})
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to load customer email")
		return
	}
	if email != "" {
		profile.Email = &email
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
			methods, ok := paymentMethodsWithCollectionDefaults(r, customerID, pms)
			if !ok {
				return
			}
			profile.PaymentMethods = methods
		}
	}
	if r.State.MoneyService != nil {
		payer := identity.CustomerIDFromString(path.UserID)
		if !payer.IsZero() {
			// or#864: no invented currency. Trust level is per (payer,
			// currency); an absent query param used to silently mean USD, so a
			// EUR-only customer's profile reported a USD trust level as if it
			// were theirs. The top-level field is now populated ONLY when the
			// caller names a currency, and every balance carries its OWN trust
			// level below — strictly more information, none of it guessed.
			if currency := strings.TrimSpace(r.Query("currency")); currency != "" {
				if err := moneyutil.ValidateCurrency(currency); err != nil {
					r.ErrorJSON(http.StatusBadRequest, err.Error())
					return
				}
				if trustLevel, err := r.State.MoneyService.GetTrustLevel(ctx, payer, currency); err == nil {
					profile.TrustLevel = trustLevel
				}
			}
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
				balanceTrust := ""
				if tl, err := r.State.MoneyService.GetTrustLevel(ctx, payer, bal.Currency); err == nil {
					balanceTrust = tl
				}
				profile.CreditBalance = append(profile.CreditBalance, adminCreditBalanceResponse{
					Currency:              bal.Currency,
					TrustLevel:            balanceTrust,
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

func GetAdminUserPaymentMethods(r *httprequest.Request) {
	var path adminUserPath
	if err := r.ShouldBindURI(&path); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	if r.State.PaymentMethodService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "payment method service unavailable")
		return
	}
	pms, err := r.State.PaymentMethodService.GetByUserID(r.Request.Context(), path.UserID)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to load payment methods")
		return
	}
	methods, ok := paymentMethodsWithCollectionDefaults(r, identity.CustomerIDFromString(path.UserID), pms)
	if !ok {
		return
	}
	r.SuccessJSON(map[string]any{"object": "list", "data": methods})
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
	if err := r.State.AdminSubscriptionService.CancelSubscription(r.Request.Context(), subscriptionID, req.Reason, req.RevokeAccess); err != nil {
		// Map domain errors to stable status codes; never leak raw sql/pgx text
		// to the client (#783). AdminSubscriptionService.CancelSubscription
		// returns "subscription not found: <wrapped ErrNoRows>" for a missing
		// id, "subscription is not active" for a bad state, and "cancel
		// operation not supported for rail '<rail>'" for an ineligible rail.
		msg := err.Error()
		switch {
		case strings.HasPrefix(msg, "subscription not found"):
			r.ErrorJSON(http.StatusNotFound, "subscription not found")
		case strings.Contains(msg, "not active"):
			r.ErrorJSON(http.StatusConflict, "subscription is not active")
		// or#896: a Solana cancel is the subscriber's signature to give, not a
		// server-side operation — name the endpoints instead of a 500.
		case errors.Is(err, subscriptions.ErrSolanaCancelNeedsWalletSignature):
			r.ErrorJSON(http.StatusBadRequest, msg)
		case strings.Contains(msg, "not supported for rail"):
			r.ErrorJSON(http.StatusBadRequest, msg)
		default:
			log.WithError(err).Error("admin cancel subscription failed")
			r.ErrorJSON(http.StatusInternalServerError, "failed to cancel subscription")
		}
		return
	}
	r.SuccessJSONMessage("subscription cancelled successfully")
}

func AdminResumeSubscription(r *httprequest.Request) {
	subscriptionID, err := api.ParseSubscriptionID(r.Param("id"))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid subscription ID")
		return
	}
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
