package engine

import (
	"fmt"
	"io/fs"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"

	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/paymentpagehttp"
	"github.com/open-rails/openrails/internal/paymentpage"
)

// serveCheckout builds the payment page Config.CheckoutOrigin declares, from
// assets. An origin that is not its own, or assets without the page, are
// refused alone and reported; nil assets issue pay URLs without serving the
// page.
func (e *Engine) serveCheckout(cfg billing.Config, declarations []billing.MerchantDeclaration, assets fs.FS) {
	if strings.TrimSpace(cfg.CheckoutOrigin) == "" {
		return
	}
	forbidden := []string{cfg.PublicBillingBaseURL}
	for _, m := range declarations {
		forbidden = append(forbidden, m.Issuer)
	}
	rt := e.App.Runtime
	origin, err := paymentpage.Origin(cfg.CheckoutOrigin, forbidden...)
	if err != nil {
		rt.Refuse("checkout_origin", fmt.Errorf("%w; no payment page is served and orders get no pay URL", err))
		return
	}
	if assets == nil {
		rt.SetCheckoutOrigin(origin)
		e.checkoutOrigin = origin
		return
	}
	limiter := middleware.RateLimitHTTP(e.Log, rt.Config.RateLimits, rt.Config.Captcha, rt.AbuseState, rt.CaptchaStore, rt.TrustedProxies)
	if e.checkoutOrigin, e.checkout, err = paymentpagehttp.New(rt, origin, assets, nil, limiter); err != nil {
		rt.Refuse("checkout_assets", fmt.Errorf("%w; no payment page is served and orders get no pay URL", err))
	}
}

// CheckoutPage is the payment page's origin and handler: serve the handler
// only on that origin (openrailsgin.CheckoutOrigin does). "" and nil when
// Config declares none or refused it; the origin and a nil handler without
// Deps.CheckoutAssets.
func (e *Engine) CheckoutPage() (origin string, handler http.Handler) {
	return e.checkoutOrigin, e.checkout
}
