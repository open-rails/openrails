package iputil

import (
	"net/http/httptest"
	"testing"
)

func TestCloudflarePeersAloneSetTheCountryAndClientIP(t *testing.T) {
	proxies := ParseTrustedProxies([]string{"10.0.0.0/8"})
	for name, c := range map[string]struct {
		peer, xff, country, clientIP string
	}{
		"a Cloudflare peer":                  {"173.245.48.7:443", "203.0.113.9", "DE", "203.0.113.9"},
		"a Cloudflare IPv6 peer":             {"[2606:4700::1]:443", "203.0.113.9", "DE", "203.0.113.9"},
		"Cloudflare behind the ingress":      {"10.1.2.3:80", "203.0.113.9, 173.245.48.7", "DE", "203.0.113.9"},
		"a peer that is not Cloudflare's":    {"198.51.100.4:443", "203.0.113.9", "", "198.51.100.4"},
		"the ingress, not Cloudflare":        {"10.1.2.3:80", "198.51.100.4", "", "198.51.100.4"},
		"a client claiming to be Cloudflare": {"10.1.2.3:80", "173.245.48.7, 198.51.100.4", "", "198.51.100.4"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.peer
		r.Header.Set("X-Forwarded-For", c.xff)
		r.Header.Set("CF-IPCountry", "de")
		if got := proxies.Country(r); got != c.country {
			t.Errorf("%s: country %q, want %q", name, got, c.country)
		}
		if got := proxies.ClientIP(r); got != c.clientIP {
			t.Errorf("%s: client IP %q, want %q", name, got, c.clientIP)
		}
	}
}
