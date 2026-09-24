package handlers

import (
	"net/http"
	"slices"
	"strings"

	"github.com/open-rails/openrails/config"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

type portalResponse struct {
	URL string `json:"url"`
}

func CreatePortalSession(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil || user.ID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "User authentication required")
		return
	}
	customerID, err := r.State.RailCustomerService.GetCustomerID(r.Request.Context(), user.ID, "stripe")
	if err != nil || strings.TrimSpace(customerID) == "" {
		r.ErrorJSON(http.StatusNotFound, "stripe customer not found")
		return
	}
	returnURL := portalReturnOrigin(r)
	if returnURL == "" {
		r.ErrorJSON(http.StatusBadRequest, "return_url unavailable")
		return
	}
	returnURL += "/account"
	service := &subscriptions.StripePortalService{StripeClients: r.State.StripeClients, Config: r.State.Config, Rails: r.State.RailConfigs}
	urlStr, err := service.CreatePortalSession(r.Request.Context(), customerID, returnURL)
	if err != nil {
		writeRefusal(r, err, "billing portal unavailable")
		return
	}
	r.SuccessJSON(portalResponse{URL: urlStr})
}

// portalReturnOrigin returns the browser's origin only when it is an allowed
// return origin (SEC-33), else the first allowed origin. Request headers never
// choose an unlisted destination.
func portalReturnOrigin(r *httprequest.Request) string {
	if r == nil || r.State == nil || r.State.Config == nil {
		return ""
	}
	allowed := r.State.Config.AllowedReturnOrigins()
	if len(allowed) == 0 {
		return ""
	}
	if origin, ok := config.URLOrigin(r.Header("Origin")); ok && slices.Contains(allowed, origin) {
		return origin
	}
	return allowed[0]
}
