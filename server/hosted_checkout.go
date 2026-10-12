package server

import (
	"io/fs"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/internal/hostedcheckout"
	checkoutpage "github.com/open-rails/openrails/web/checkout"
)

// checkoutOrigin is Config.HostedCheckoutURL as an origin, "" for none. The
// page runs the PSPs' scripts, so it never shares an origin with the API,
// the admin console, AuthKit or the product's pages (Stripe keeps
// checkout.stripe.com apart from its dashboard).
func checkoutOrigin(cfg Config) (string, error) {
	if strings.TrimSpace(cfg.HostedCheckoutURL) == "" {
		return "", nil
	}
	forbidden := []string{cfg.Engine.PublicBillingBaseURL, cfg.Auth.Token.Issuer, cfg.Auth.Frontend.BaseURL}
	if cfg.Auth.HTTP != nil {
		forbidden = append(forbidden, cfg.Auth.HTTP.PublicURL)
	}
	return hostedcheckout.Origin(cfg.HostedCheckoutURL, forbidden...)
}

// checkoutAssets is the host's checkout page build, else the one embedded
// in this binary.
func checkoutAssets(assets fs.FS) fs.FS {
	if assets != nil {
		return assets
	}
	return checkoutpage.FS()
}

// HostedCheckout is the hosted checkout host and its whole surface (the page
// and the routes its secret opens), for a host that mounts Routes on its own
// router: it sends requests for that host here. "" and nil without one.
func (s *Server) HostedCheckout() (host string, handler http.Handler) {
	return s.surface.HostedCheckout()
}
