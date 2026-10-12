package engine

import (
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"strings"

	"github.com/open-rails/openrails/internal/hostedcheckout"
	"github.com/open-rails/openrails/internal/http/checkouthttp"
	"github.com/open-rails/openrails/internal/http/middleware"
)

// HostedCheckout installs the embedded engine's one isolated checkout origin.
// Like route composition, this happens before workers or HTTP start.
func (e *Engine) HostedCheckout(origin string, assets fs.FS) (string, http.Handler, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return "", nil, fmt.Errorf("openrails: engine is closed")
	}
	if e.App.Standalone {
		return "", nil, fmt.Errorf("openrails: standalone checkout belongs to server.HostedCheckout")
	}
	if e.stopWorkers != nil {
		return "", nil, fmt.Errorf("openrails: configure hosted checkout before Start")
	}
	if e.App.Runtime.HostedCheckoutOrigin != "" {
		return "", nil, fmt.Errorf("openrails: hosted checkout is already configured")
	}
	forbidden := []string{e.App.Config.PublicBillingBaseURL, e.App.Config.DashboardBaseURL}
	for _, merchant := range e.App.Config.Merchants {
		forbidden = append(forbidden, merchant.Issuer)
	}
	origin, err := hostedcheckout.Origin(origin, forbidden...)
	if err != nil {
		return "", nil, err
	}
	rt := e.App.Runtime
	if assets == nil {
		rt.HostedCheckoutOrigin = origin
		u, _ := url.Parse(origin)
		return strings.ToLower(u.Host), nil, nil
	}
	limiter := middleware.RateLimitHTTP(rt.Config.RateLimits, rt.Config.Captcha, rt.AbuseState, rt.CaptchaStore, rt.TrustedProxies)
	return checkouthttp.New(rt, origin, assets, nil, limiter)
}
