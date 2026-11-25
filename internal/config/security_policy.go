package config

import (
	"net/url"
	"slices"
	"strings"

	"github.com/open-rails/openrails/billing"
)

// ReturnOrigins are the exact origins one merchant's return URLs may name
// (SEC-33), in this order: the billing origin (PublicBillingBaseURL's), the
// origin of the merchant's declared Issuer, and its checkout.embed_origins.
// Nothing else, so one merchant's checkout never returns to another's site.
func ReturnOrigins(cfg *billing.Config, issuer string, embedOrigins []string) []string {
	var out []string
	add := func(raw string) {
		if origin, ok := URLOrigin(raw); ok && !slices.Contains(out, origin) {
			out = append(out, origin)
		}
	}
	if cfg != nil {
		add(cfg.PublicBillingBaseURL)
	}
	add(issuer)
	for _, raw := range embedOrigins {
		add(raw)
	}
	return out
}

// PortalReturnOrigin is where a merchant's billing portal returns: browser,
// the request's Origin, when it is one of the merchant's return origins, else
// its issuer's origin, else the billing origin; "" when there is none.
func PortalReturnOrigin(cfg *billing.Config, issuer string, embedOrigins []string, browser string) string {
	if origin, ok := URLOrigin(browser); ok && slices.Contains(ReturnOrigins(cfg, issuer, embedOrigins), origin) {
		return origin
	}
	if origin, ok := URLOrigin(issuer); ok {
		return origin
	}
	origin, _ := URLOrigin(cfgBillingBase(cfg))
	return origin
}

func cfgBillingBase(cfg *billing.Config) string {
	if cfg == nil {
		return ""
	}
	return cfg.PublicBillingBaseURL
}

// ReturnURLAllowed reports whether raw is an absolute URL whose origin is
// exactly one of allowed.
func ReturnURLAllowed(allowed []string, raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User != nil || u.Opaque != "" {
		return false
	}
	origin, ok := URLOrigin(raw)
	return ok && slices.Contains(allowed, origin)
}

// URLOrigin returns scheme://host[:port] in lower case for an absolute http(s) URL.
func URLOrigin(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", false
	}
	return strings.ToLower(u.Scheme + "://" + u.Host), true
}
