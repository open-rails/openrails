package iputil

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// #746: proxy trust is opt-in; an untrusted peer's X-Forwarded-For has no effect.
// Every X-Forwarded-For line counts, in order.
func TestClientIPTrustsOnlyConfiguredProxies(t *testing.T) {
	lb := []string{"10.0.0.0/8", " fd00::/8 "}
	for _, tc := range []struct {
		name   string
		cidrs  []string
		remote string
		ff     []string
		want   string
	}{
		{"nothing trusted", nil, "10.0.0.5:443", []string{"203.0.113.9"}, "10.0.0.5"},
		{"untrusted peer spoofing", lb, "203.0.113.9:443", []string{"64.38.212.5"}, "203.0.113.9"},
		{"trusted peer single hop", lb, "10.0.0.5:443", []string{"64.38.212.5"}, "64.38.212.5"},
		{"walks right to left past trusted hops", lb, "10.0.0.5:443", []string{"198.51.100.1, 64.38.212.5 , 10.0.0.6"}, "64.38.212.5"},
		{"a proxy's own line follows the client's", lb, "10.0.0.5:443", []string{"64.38.212.5", "203.0.113.9"}, "203.0.113.9"},
		{"lines join in order past trusted hops", lb, "10.0.0.5:443", []string{"198.51.100.1", "64.38.212.5, 10.0.0.6", "10.0.0.7"}, "64.38.212.5"},
		{"all hops trusted falls back to peer", lb, "10.0.0.5:443", []string{"10.0.0.6,10.0.0.7"}, "10.0.0.5"},
		{"trusted peer without header", lb, "10.0.0.5:443", nil, "10.0.0.5"},
		{"ipv6 trusted peer", lb, "[fd00::1]:443", []string{"2001:db8::7"}, "2001:db8::7"},
		{"bare host remote addr", nil, "203.0.113.9", nil, "203.0.113.9"},
		{"malformed cidr trusts nothing", []string{"not-a-cidr", ""}, "10.0.0.5:443", []string{"64.38.212.5"}, "10.0.0.5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", nil)
			r.RemoteAddr = tc.remote
			for _, line := range tc.ff {
				r.Header.Add("X-Forwarded-For", line)
			}
			require.Equal(t, tc.want, ParseTrustedProxies(tc.cidrs).ClientIP(r))
		})
	}
	var nilResolver *TrustedProxies
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.RemoteAddr = "10.0.0.5:443"
	r.Header.Set("X-Forwarded-For", "64.38.212.5")
	require.Equal(t, "10.0.0.5", nilResolver.ClientIP(r))
}

func TestSourceAllowlists(t *testing.T) {
	for ip, want := range map[string]bool{
		"64.38.212.1": true, "64.38.215.9": true, "64.38.240.10": true, "64.38.241.254": true,
		"64.38.213.1": false, "203.0.113.7": false, "": false, "not-an-ip": false,
		"64.38.212.1, 203.0.113.9": false, // a raw forwarded list is never an address
	} {
		require.Equal(t, want, IsValidCCBillIP(ip), ip)
	}
	cidrs := []string{"bogus", " 192.0.2.0/24 "}
	require.True(t, IPInAnyCIDR(" 192.0.2.7 ", cidrs))
	require.False(t, IPInAnyCIDR("192.0.3.7", cidrs))
	require.False(t, IPInAnyCIDR("192.0.2.7", nil))
	require.False(t, IPInAnyCIDR("nope", cidrs))
}

// A card is accepted in live posture only over HTTPS (#1129): the proxy's
// word counts only when the socket peer is a trusted proxy.
func TestForwardedHTTPSTrustsOnlyConfiguredProxies(t *testing.T) {
	proxies := ParseTrustedProxies([]string{"10.0.0.0/8"})
	for name, tc := range map[string]struct {
		t      *TrustedProxies
		remote string
		protos []string
		want   bool
	}{
		"trusted proxy says https":     {proxies, "10.0.0.5:443", []string{"https"}, true},
		"trusted proxy says http":      {proxies, "10.0.0.5:443", []string{"http"}, false},
		"trusted proxy says nothing":   {proxies, "10.0.0.5:443", nil, false},
		"client line beside the proxy": {proxies, "10.0.0.5:443", []string{"https", "http"}, false},
		"client value inside one line": {proxies, "10.0.0.5:443", []string{"https, http"}, false},
		"every hop https":              {proxies, "10.0.0.5:443", []string{"https, HTTPS"}, true},
		"untrusted peer claims https":  {proxies, "203.0.113.9:443", []string{"https"}, false},
		"no proxies configured":        {nil, "10.0.0.5:443", []string{"https"}, false},
	} {
		r := httptest.NewRequest(http.MethodPost, "http://billing.test/v1/me/payment-methods", nil)
		r.RemoteAddr = tc.remote
		for _, proto := range tc.protos {
			r.Header.Add("X-Forwarded-Proto", proto)
		}
		require.Equal(t, tc.want, tc.t.ForwardedHTTPS(r), name)
	}
}
