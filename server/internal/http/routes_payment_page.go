package server

import (
	"io/fs"
	"net/http"

	"github.com/open-rails/openrails/internal/http/paymentpagehttp"
	"github.com/open-rails/openrails/internal/paymentpage"
)

// buildPaymentPage serves the payment page at origin: the page, and the
// routes a pay URL's secret opens, under the host's own headers. The
// public handler then sends that host's requests here and nothing else.
func (s *Server) buildPaymentPage(origin string, assets fs.FS) error {
	if origin == "" {
		return nil
	}
	origin, handler, err := paymentpagehttp.New(s.runtime, origin, assets, nil, s.sharedRateLimit)
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
