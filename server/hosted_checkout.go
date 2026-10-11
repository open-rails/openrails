package server

import (
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"

	checkoutpage "github.com/open-rails/openrails/web/checkout"
)

// checkoutOrigin is Config.HostedCheckoutURL as an origin, "" for none. The
// page runs the PSPs' scripts, so it never shares an origin with the API,
// the admin console, AuthKit or the product's pages (Stripe keeps
// checkout.stripe.com apart from its dashboard).
func checkoutOrigin(cfg Config) (string, error) {
	raw := strings.TrimSpace(cfg.HostedCheckoutURL)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") ||
		!(u.Scheme == "https" || u.Scheme == "http" && isLoopback(u.Hostname())) {
		return "", fmt.Errorf("server: hosted_checkout.url %q must be an https origin with no path (http on loopback)", raw)
	}
	origin := strings.ToLower(u.Scheme + "://" + u.Host)
	others := []struct{ name, url string }{
		{"public_billing_base_url", cfg.Engine.PublicBillingBaseURL}, {"auth.token.issuer", cfg.Auth.Token.Issuer},
		{"auth.frontend.base_url", cfg.Auth.Frontend.BaseURL},
	}
	if cfg.Auth.HTTP != nil {
		others = append(others, struct{ name, url string }{"auth.http.public_url", cfg.Auth.HTTP.PublicURL})
	}
	for _, other := range others {
		o, err := url.Parse(strings.TrimSpace(other.url))
		if err == nil && o.Host != "" && strings.ToLower(o.Scheme+"://"+o.Host) == origin {
			return "", fmt.Errorf("server: hosted_checkout.url %s is %s's origin; serve the checkout page on its own origin", origin, other.name)
		}
	}
	return origin, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// checkoutAssets is the host's checkout page build, else the one embedded
// in this binary.
func checkoutAssets(assets fs.FS) fs.FS {
	if assets != nil {
		return assets
	}
	return checkoutpage.FS()
}

// HostedCheckout is the hosted checkout host and its whole surface (the page
// and the routes its secret opens), for a host that mounts Routes on its own
// router: it sends requests for that host here. "" and nil without one.
func (s *Server) HostedCheckout() (host string, handler http.Handler) {
	return s.surface.HostedCheckout()
}
