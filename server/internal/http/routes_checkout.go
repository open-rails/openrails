package server

import (
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/hostedcheckout"
	"github.com/open-rails/openrails/internal/http/embedhttp"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
)

// buildCheckout serves the hosted checkout at origin: the page, and the
// routes a checkout URL's secret opens, under the host's own headers. The
// public handler then sends that host's requests here and nothing else.
func (s *Server) buildCheckout(origin string, assets fs.FS) error {
	if origin == "" {
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return fmt.Errorf("hosted checkout origin %q", origin)
	}
	page, err := hostedcheckout.Page(assets)
	if err != nil {
		return fmt.Errorf("%w: build it (`task checkout-build`) before go build, or unset hosted_checkout.url", err)
	}
	mux := &router.Table{}
	httproutes.RegisterCheckoutRoutes(router.NewMux(mux, "", s.runtime), s.runtime, httproutes.Options{
		Capabilities: &billing.Capabilities{RouteGroups: map[string]bool{}, Features: map[string]bool{}},
		External: httproutes.External{
			CaptchaStatus: embedhttp.CaptchaStatusHandler(s.cfg.Captcha, s.captchaStore, s.trustedProxies()),
			CaptchaScript: embedhttp.CaptchaClientScriptHandler(s.cfg.Captcha),
		},
	})
	mux.Handle("GET /c/{order_id}", page)
	mux.Handle("GET /assets/{file...}", page)
	s.checkoutHost = strings.ToLower(u.Host)
	s.checkoutHandler = middleware.ChainHTTP(mux.Handler(),
		middleware.RecoverHTTP(),
		middleware.RequestLogHTTP(),
		hostedcheckout.HeadersHTTP,
		middleware.RequestLimitsHTTP(middleware.DefaultMaxBodyBytes),
		billingauth.ExplicitCredentials,
		hostedcheckout.ResolveHTTP(hostedcheckout.NewStore(s.runtime.DB), s.runtime.Clock.Now),
		s.sharedRateLimit,
	)
	s.runtime.HostedCheckoutOrigin = origin
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
