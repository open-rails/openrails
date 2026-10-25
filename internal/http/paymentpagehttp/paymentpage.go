// Package paymentpagehttp mounts the shared hosted order payment page surface.
package paymentpagehttp

import (
	"fmt"
	"io/fs"
	"net/http"
	"net/url"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/paymentpage"
)

// New builds the same capability-bound surface for standalone and embedded
// hosts. The caller validates the origin against its identity and API origins
// before publishing this handler or starting engine workers.
func New(rt *app.Runtime, origin string, assets fs.FS, issuers []string, limiter func(http.Handler) http.Handler) (string, http.Handler, error) {
	if u, err := url.Parse(origin); err != nil || u.Host == "" {
		return "", nil, fmt.Errorf("checkout origin %q", origin)
	}
	page, err := paymentpage.Page(assets)
	if err != nil {
		return "", nil, err
	}
	mux := &router.Table{}
	httproutes.RegisterPaymentPageRoutes(router.NewMux(mux, "", rt), rt, httproutes.Options{Capabilities: &billing.Capabilities{RouteGroups: map[string]bool{}, Features: map[string]bool{}}})
	captcha := ""
	if config.CaptchaEnabled(rt.Config.Captcha) {
		captcha = config.CaptchaProvider(rt.Config.Captcha)
	}
	store := paymentpage.NewStore(rt.DB)
	var settings paymentpage.MerchantSettings
	if rt.Merchants != nil {
		settings = rt.Merchants
	}
	mux.Handle("GET /pay/{order_id}", page)
	mux.Handle("GET /assets/{file...}", page)
	mux.Handle("GET "+paymentpage.ApplePayDomainAssociationPath, paymentpage.ApplePayDomainAssociation())
	handler := middleware.ChainHTTP(mux.Handler(),
		middleware.RecoverHTTP(), middleware.RequestLogHTTP(rt.TrustedProxies),
		paymentpage.HeadersHTTP(paymentpage.ContentSecurityPolicy(captcha, issuers...), store.Framing(settings)),
		middleware.RequestLimitsHTTP(middleware.DefaultMaxBodyBytes), billingauth.ExplicitCredentials,
		paymentpage.ResolveHTTP(store, rt.Clock.Now), limiter,
	)
	rt.SetCheckoutOrigin(origin)
	return origin, handler, nil
}
