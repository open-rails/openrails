package server

import (
	"io/fs"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/internal/paymentpage"
	paypage "github.com/open-rails/openrails/web/pay"
)

// paymentOrigin is Config.PaymentOrigin as an origin, "" for none. The
// page runs the PSPs' scripts, so it never shares an origin with the API,
// the admin console, AuthKit or the product's pages (Stripe keeps
// checkout.stripe.com apart from its dashboard).
func paymentOrigin(cfg Config) (string, error) {
	if strings.TrimSpace(cfg.PaymentOrigin) == "" {
		return "", nil
	}
	forbidden := []string{cfg.Engine.PublicBillingBaseURL, cfg.Auth.Token.Issuer, cfg.Auth.Frontend.BaseURL}
	if cfg.Auth.HTTP != nil {
		forbidden = append(forbidden, cfg.Auth.HTTP.PublicURL)
	}
	return paymentpage.Origin(cfg.PaymentOrigin, forbidden...)
}

// paymentPageAssets is the host's payment page build, else the one embedded
// in this binary.
func paymentPageAssets(assets fs.FS) fs.FS {
	if assets != nil {
		return assets
	}
	return paypage.FS()
}

// PaymentPage is the payment origin and its whole surface (the page and the
// routes a pay URL's secret opens), for a host that mounts Routes on its own
// router: it sends that origin's requests here (openrailsgin.PaymentOrigin).
// "" and nil without one.
func (s *Server) PaymentPage() (origin string, handler http.Handler) {
	return s.surface.PaymentPage()
}
