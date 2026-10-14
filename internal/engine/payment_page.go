package engine

import (
	"fmt"
	"io/fs"
	"net/http"

	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/paymentpagehttp"
	"github.com/open-rails/openrails/internal/paymentpage"
)

// CheckoutPage installs the embedded engine's one isolated checkout origin.
// Like route composition, this happens before workers or HTTP start.
func (e *Engine) CheckoutPage(origin string, assets fs.FS) (string, http.Handler, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return "", nil, fmt.Errorf("openrails: engine is closed")
	}
	if e.App.Standalone {
		return "", nil, fmt.Errorf("openrails: a standalone payment page belongs to server.CheckoutPage")
	}
	if e.stopWorkers != nil {
		return "", nil, fmt.Errorf("openrails: configure the payment page before Start")
	}
	if e.App.Runtime.CheckoutOrigin() != "" {
		return "", nil, fmt.Errorf("openrails: the payment page is already configured")
	}
	forbidden := []string{e.App.Config.PublicBillingBaseURL, e.App.Config.DashboardBaseURL}
	for _, merchant := range e.App.Config.Merchants {
		forbidden = append(forbidden, merchant.Issuer)
	}
	origin, err := paymentpage.Origin(origin, forbidden...)
	if err != nil {
		return "", nil, err
	}
	rt := e.App.Runtime
	if assets == nil {
		rt.SetCheckoutOrigin(origin)
		return origin, nil, nil
	}
	limiter := middleware.RateLimitHTTP(rt.Config.RateLimits, rt.Config.Captcha, rt.AbuseState, rt.CaptchaStore, rt.TrustedProxies)
	return paymentpagehttp.New(rt, origin, assets, nil, limiter)
}
