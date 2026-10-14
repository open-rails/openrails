package server

import (
	"io/fs"
	"net/http"
	"net/url"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/paymentpagehttp"
	"github.com/open-rails/openrails/internal/paymentpage"
	"github.com/open-rails/openrails/server/internal/network"
)

// buildPaymentPage serves the payment page at origin: the page, and the
// routes a pay URL's secret opens, under the host's own headers. The
// public handler then sends that host's requests here and nothing else.
func (s *Server) buildPaymentPage(origin string, assets fs.FS) error {
	if origin == "" {
		return nil
	}
	var issuers []string
	if s.runtime.Network != nil {
		if iss, err := url.Parse(s.controlPlane.Issuer()); err == nil && iss.Host != "" {
			issuers = append(issuers, iss.Scheme+"://"+iss.Host)
			s.runtime.PaymentPageNetwork = &billing.PublicNetwork{Issuer: s.controlPlane.Issuer(), ClientID: network.PaymentPageClientID}
		}
	}
	origin, handler, err := paymentpagehttp.New(s.runtime, origin, assets, issuers, s.sharedRateLimit)
	if err != nil {
		return err
	}
	host, err := paymentpage.NewHost(origin)
	if err != nil {
		return err
	}
	s.checkoutOrigin, s.paymentHost, s.paymentHandler = origin, host, handler
	return nil
}

// dispatchPaymentPage sends the checkout origin's requests to its surface
// and nothing else there; a malformed Host is refused.
func (s *Server) dispatchPaymentPage(next http.Handler) http.Handler {
	if s.paymentHandler == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch match, valid := s.paymentHost.Match(r.Host); {
		case !valid:
			http.Error(w, "bad request", http.StatusBadRequest)
		case match:
			s.paymentHandler.ServeHTTP(w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// CheckoutPage is the checkout origin and its surface; "" and nil without one.
func (s *Server) CheckoutPage() (string, http.Handler) {
	return s.checkoutOrigin, s.paymentHandler
}
