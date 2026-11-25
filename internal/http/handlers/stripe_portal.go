package handlers

import (
	"strings"

	"github.com/open-rails/openrails/billing"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// StripePortalSession is where the customer manages the subscriptions Stripe
// runs: Stripe's own customer portal.
type StripePortalSession struct {
	URL string `json:"url"`
}

// CreateStripePortalSession opens Stripe's customer portal. A merchant
// without an armed Stripe PSP does not serve the route.
func CreateStripePortalSession(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil || user.ID == "" {
		r.ErrorCode(billing.CodeAuthenticationRequired, "User authentication required")
		return
	}
	if r.State.RailConfigs == nil {
		r.ErrorCode(billing.CodeRouteNotFound, "")
		return
	}
	armed, err := r.State.RailConfigs.Armed(r.Request.Context(), string(models.RailStripe))
	if err != nil {
		r.InternalError("stripe configuration unavailable", err)
		return
	}
	if !armed {
		r.ErrorCode(billing.CodeRouteNotFound, "")
		return
	}
	customerID, err := r.State.RailCustomerService.GetCustomerID(r.Request.Context(), user.ID, string(models.RailStripe))
	if err != nil || strings.TrimSpace(customerID) == "" {
		r.ErrorCode(billing.CodeResourceNotFound, "stripe customer not found")
		return
	}
	returnURL := portalReturnOrigin(r)
	if returnURL == "" {
		r.ErrorCode(billing.CodeInvalidParam, "return_url unavailable")
		return
	}
	returnURL += "/account"
	service := &subscriptions.StripePortalService{StripeClients: r.State.StripeClients, Config: r.State.Config, Rails: r.State.RailConfigs}
	urlStr, err := service.CreatePortalSession(r.Request.Context(), customerID, returnURL)
	if err != nil {
		writeRefusal(r, err, "billing portal unavailable")
		return
	}
	r.SuccessJSON(StripePortalSession{URL: urlStr})
}

// portalReturnOrigin is the browser's origin when it is one of the merchant's
// return origins, else the merchant's issuer origin, else the billing
// origin. Request headers never choose another merchant's site.
func portalReturnOrigin(r *httprequest.Request) string {
	if r == nil || r.State == nil {
		return ""
	}
	ctx := r.Request.Context()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return ""
	}
	embed, err := r.State.EmbedOrigins(ctx, mid)
	if err != nil {
		return ""
	}
	return config.PortalReturnOrigin(r.State.Config, r.State.MerchantIssuer(mid), embed, r.Header("Origin"))
}
