package merchantconfig

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/open-rails/openrails/billing"
)

// MaxEmbedOrigins bounds checkout.embed_origins.
const MaxEmbedOrigins = 10

// NormalizeEmbedOrigins validates checkout.embed_origins: each an exact
// origin (scheme, host and port; no path, wildcard or credentials), https or
// http on loopback, at most MaxEmbedOrigins, none twice. They become a CSP
// frame-ancestors source list, so nothing broader is accepted.
func NormalizeEmbedOrigins(in []string) ([]string, error) {
	if len(in) > MaxEmbedOrigins {
		return nil, fmt.Errorf("checkout.embed_origins: at most %d origins", MaxEmbedOrigins)
	}
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, raw := range in {
		origin, ok := embedOrigin(raw)
		if !ok {
			return nil, fmt.Errorf("checkout.embed_origins: %q is not an exact https origin (http on loopback), like https://shop.example", raw)
		}
		if seen[origin] {
			return nil, fmt.Errorf("checkout.embed_origins: %q is listed twice", raw)
		}
		seen[origin] = true
		out = append(out, origin)
	}
	return out, nil
}

// EmbedOrigins are the origins settings let frame the merchant's embedded
// checkouts; none when they name none or name them invalidly.
func EmbedOrigins(settings billing.MerchantSettings) []string {
	if settings.Checkout == nil {
		return []string{}
	}
	out, err := NormalizeEmbedOrigins(settings.Checkout.EmbedOrigins)
	if err != nil {
		return []string{}
	}
	return out
}

func embedOrigin(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") || strings.ContainsAny(raw, "*;, ") {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && loopback(host):
	default:
		return "", false
	}
	origin := u.Scheme + "://" + strings.ToLower(u.Host)
	return origin, true
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
