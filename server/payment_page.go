package server

import (
	"net/http"
	"strings"

	"github.com/open-rails/authkit"

	"github.com/open-rails/openrails/internal/paymentpage"
)

// checkCheckoutOrigin refuses an Engine.CheckoutOrigin that is not an origin
// of its own. The page runs the PSPs' scripts, so it never shares an origin
// with the API, the admin console, AuthKit or the product's pages (Stripe
// keeps checkout.stripe.com apart from its dashboard).
func checkCheckoutOrigin(cfg Config, auth authkit.Config) error {
	if strings.TrimSpace(cfg.Engine.CheckoutOrigin) == "" {
		return nil
	}
	forbidden := []string{cfg.Engine.PublicURL, auth.Token.Issuer, auth.Frontend.BaseURL}
	if auth.HTTP != nil {
		forbidden = append(forbidden, auth.HTTP.PublicURL)
	}
	_, err := paymentpage.Origin(cfg.Engine.CheckoutOrigin, forbidden...)
	return err
}

// CheckoutPage is the checkout origin and its whole surface (the page and the
// routes a pay URL's secret opens), for a host that mounts Routes on its own
// router: it sends that origin's requests here (openrailsgin.CheckoutOrigin).
// "" and nil without one.
func (s *Server) CheckoutPage() (origin string, handler http.Handler) {
	return s.surface.CheckoutPage()
}
