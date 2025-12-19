package handlers

import (
	"net/http"
	"net/url"
	"strings"

	authgin "github.com/PaulFidika/authkit/adapters/gin"
	"github.com/doujins-org/doujins-billing/internal/services"
)

type portalResponse struct {
	RedirectURL string `json:"redirect_url"`
}

// CreatePortalSession creates a Stripe customer portal session.
// POST /v1/me/portal
func CreatePortalSession(r *Request) {
	cl, ok := authgin.ClaimsFromGin(r.GinCtx)
	if !ok || cl.UserID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "User authentication required")
		return
	}
	if r.State == nil || r.State.ProcessorCustomerService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "portal service unavailable")
		return
	}
	customerID, err := r.State.ProcessorCustomerService.GetCustomerID(r.Request.Context(), cl.UserID, "stripe")
	if err != nil || strings.TrimSpace(customerID) == "" {
		r.ErrorJSON(http.StatusNotFound, "stripe customer not found")
		return
	}
	returnURL := guessBaseURLPortal(r.Request)
	if returnURL == "" {
		r.ErrorJSON(http.StatusBadRequest, "return_url unavailable")
		return
	}
	returnURL = returnURL + "/account"
	service := &services.StripePortalService{Config: r.State.Config}
	urlStr, err := service.CreatePortalSession(r.Request.Context(), customerID, returnURL)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.SuccessJSON(portalResponse{RedirectURL: urlStr})
}

func guessBaseURLPortal(req *http.Request) string {
	if req == nil {
		return ""
	}
	if origin := strings.TrimSpace(req.Header.Get("Origin")); origin != "" {
		if u, err := url.Parse(origin); err == nil && u.Scheme != "" && u.Host != "" {
			return u.Scheme + "://" + u.Host
		}
	}
	if ref := strings.TrimSpace(req.Header.Get("Referer")); ref != "" {
		if u, err := url.Parse(ref); err == nil && u.Scheme != "" && u.Host != "" {
			return u.Scheme + "://" + u.Host
		}
	}
	scheme := strings.TrimSpace(req.Header.Get("X-Forwarded-Proto"))
	if scheme == "" {
		if req.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	host := strings.TrimSpace(req.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = req.Host
	}
	if host == "" || scheme == "" {
		return ""
	}
	return scheme + "://" + host
}
