package handlers

import (
	"slices"
	"strings"

	"github.com/open-rails/openrails/billing"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
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

// portalReturnOrigin returns the browser's origin only when it is an allowed
// return origin, else the first allowed origin. Request headers never choose
// an unlisted destination.
func portalReturnOrigin(r *httprequest.Request) string {
	if r == nil || r.State == nil || r.State.Config == nil {
		return ""
	}
	allowed := config.AllowedReturnOrigins(r.State.Config)
	if len(allowed) == 0 {
		return ""
	}
	if origin, ok := config.URLOrigin(r.Header("Origin")); ok && slices.Contains(allowed, origin) {
		return origin
	}
	return allowed[0]
}
