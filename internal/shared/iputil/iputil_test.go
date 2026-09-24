package iputil

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// #746: proxy trust is opt-in; an untrusted peer's X-Forwarded-For has no effect.
func TestResolveClientIPTrustsOnlyConfiguredProxies(t *testing.T) {
	lb := []string{"10.0.0.0/8", " fd00::/8 "}
	for _, tc := range []struct {
		name       string
		cidrs      []string
		remote, ff string
		want       string
	}{
		{"nothing trusted", nil, "10.0.0.5:443", "203.0.113.9", "10.0.0.5"},
		{"untrusted peer spoofing", lb, "203.0.113.9:443", "64.38.212.5", "203.0.113.9"},
		{"trusted peer single hop", lb, "10.0.0.5:443", "64.38.212.5", "64.38.212.5"},
		{"walks right to left past trusted hops", lb, "10.0.0.5:443", "198.51.100.1, 64.38.212.5 , 10.0.0.6", "64.38.212.5"},
		{"all hops trusted falls back to peer", lb, "10.0.0.5:443", "10.0.0.6,10.0.0.7", "10.0.0.5"},
		{"trusted peer without header", lb, "10.0.0.5:443", "", "10.0.0.5"},
		{"ipv6 trusted peer", lb, "[fd00::1]:443", "2001:db8::7", "2001:db8::7"},
		{"bare host remote addr", nil, "203.0.113.9", "", "203.0.113.9"},
		{"malformed cidr trusts nothing", []string{"not-a-cidr", ""}, "10.0.0.5:443", "64.38.212.5", "10.0.0.5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, ParseTrustedProxies(tc.cidrs).ResolveClientIP(tc.remote, tc.ff))
		})
	}
	var nilResolver *TrustedProxies
	require.Equal(t, "10.0.0.5", nilResolver.ResolveClientIP("10.0.0.5:443", "64.38.212.5"))
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
