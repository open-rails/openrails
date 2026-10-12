// Package checkouthttp mounts the shared hosted order checkout surface.
package checkouthttp

import (
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/hostedcheckout"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
)

// New builds the same capability-bound surface for standalone and embedded
// hosts. The caller validates the origin against its identity and API origins
// before publishing this handler or starting engine workers.
func New(rt *app.Runtime, origin string, assets fs.FS, issuers []string, limiter func(http.Handler) http.Handler) (string, http.Handler, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return "", nil, fmt.Errorf("hosted checkout origin %q", origin)
	}
	page, err := hostedcheckout.Page(assets)
	if err != nil {
		return "", nil, err
	}
	mux := &router.Table{}
	httproutes.RegisterCheckoutRoutes(router.NewMux(mux, "", rt), rt, httproutes.Options{Capabilities: &billing.Capabilities{RouteGroups: map[string]bool{}, Features: map[string]bool{}}})
	captcha := ""
	if config.CaptchaEnabled(rt.Config.Captcha) {
		captcha = config.CaptchaProvider(rt.Config.Captcha)
	}
	store := hostedcheckout.NewStore(rt.DB)
	var settings hostedcheckout.MerchantSettings
	if rt.Merchants != nil {
		settings = rt.Merchants
	}
	mux.Handle("GET /c/{order_id}", page)
	mux.Handle("GET "+hostedcheckout.SignInPath, page)
	mux.Handle("GET /assets/{file...}", page)
	mux.Handle("GET "+hostedcheckout.ApplePayDomainAssociationPath, hostedcheckout.ApplePayDomainAssociation())
	handler := middleware.ChainHTTP(mux.Handler(),
		middleware.RecoverHTTP(), middleware.RequestLogHTTP(),
		hostedcheckout.HeadersHTTP(hostedcheckout.ContentSecurityPolicy(captcha, issuers...), store.Framing(settings)),
		middleware.RequestLimitsHTTP(middleware.DefaultMaxBodyBytes), billingauth.ExplicitCredentials,
		hostedcheckout.ResolveHTTP(store, rt.Clock.Now), limiter,
	)
	rt.HostedCheckoutOrigin = origin
	return strings.ToLower(u.Host), handler, nil
}
