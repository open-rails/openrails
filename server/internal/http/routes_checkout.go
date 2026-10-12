package server

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/checkouthttp"
	"github.com/open-rails/openrails/server/internal/network"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
)

// buildCheckout serves the hosted checkout at origin: the page, and the
// routes a checkout URL's secret opens, under the host's own headers. The
// public handler then sends that host's requests here and nothing else.
func (s *Server) buildCheckout(origin string, assets fs.FS) error {
	if origin == "" {
		return nil
	}
	var issuers []string
	if s.runtime.Network != nil {
		if iss, err := url.Parse(s.controlPlane.Issuer()); err == nil && iss.Host != "" {
			issuers = append(issuers, iss.Scheme+"://"+iss.Host)
			s.runtime.CheckoutNetwork = &billing.PublicNetwork{Issuer: s.controlPlane.Issuer(), ClientID: network.CheckoutClientID}
		}
	}
	host, handler, err := checkouthttp.New(s.runtime, origin, assets, issuers, s.sharedRateLimit)
	if err != nil {
		return err
	}
	s.checkoutHost, s.checkoutHandler = host, handler
	return nil
}

// dispatchCheckout sends the checkout host's requests to its surface.
func (s *Server) dispatchCheckout(next http.Handler) http.Handler {
	if s.checkoutHandler == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Host, s.checkoutHost) {
			s.checkoutHandler.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// HostedCheckout is the checkout host and its surface; "" and nil without one.
func (s *Server) HostedCheckout() (string, http.Handler) {
	return s.checkoutHost, s.checkoutHandler
}
