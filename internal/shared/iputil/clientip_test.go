package iputil

import "testing"

func TestTrustedProxiesResolveClientIP(t *testing.T) {
	cases := []struct {
		name       string
		cidrs      []string
		remoteAddr string
		xff        string
		want       string
	}{
		{
			name:       "no trusted proxies configured trusts nothing",
			cidrs:      nil,
			remoteAddr: "10.0.0.5:443",
			xff:        "203.0.113.9",
			want:       "10.0.0.5",
		},
		{
			name:       "untrusted peer ignores forwarded header entirely (spoof attempt)",
			cidrs:      []string{"10.0.0.0/8"},
			remoteAddr: "203.0.113.9:443", // attacker, not in the trusted range
			xff:        "64.38.212.5",     // forged claim of a trusted-looking IP
			want:       "203.0.113.9",
		},
		{
			name:       "trusted peer: single-hop XFF resolves to the forwarded client",
			cidrs:      []string{"10.0.0.0/8"},
			remoteAddr: "10.0.0.5:443",
			xff:        "64.38.212.5",
			want:       "64.38.212.5",
		},
		{
			name:       "trusted peer: multi-hop XFF walks right-to-left past trusted hops",
			cidrs:      []string{"10.0.0.0/8"},
			remoteAddr: "10.0.0.5:443",
			// Rightmost (10.0.0.6) is the trusted internal LB hop; the real
			// client is the leftmost, untrusted entry.
			xff:  "64.38.212.5, 10.0.0.6",
			want: "64.38.212.5",
		},
		{
			name:       "trusted peer: every hop trusted falls back to the direct peer",
			cidrs:      []string{"10.0.0.0/8"},
			remoteAddr: "10.0.0.5:443",
			xff:        "10.0.0.6, 10.0.0.7",
			want:       "10.0.0.5",
		},
		{
			name:       "remoteAddr without a port is used as-is",
			cidrs:      nil,
			remoteAddr: "203.0.113.9",
			xff:        "",
			want:       "203.0.113.9",
		},
		{
			name:       "malformed CIDR is silently skipped, trusting nothing",
			cidrs:      []string{"not-a-cidr"},
			remoteAddr: "10.0.0.5:443",
			xff:        "64.38.212.5",
			want:       "10.0.0.5",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolver := ParseTrustedProxies(tc.cidrs)
			got := resolver.ResolveClientIP(tc.remoteAddr, tc.xff)
			if got != tc.want {
				t.Errorf("ResolveClientIP(%q, %q) = %q, want %q", tc.remoteAddr, tc.xff, got, tc.want)
			}
		})
	}
}

func TestNilTrustedProxiesTrustsNothing(t *testing.T) {
	var resolver *TrustedProxies
	got := resolver.ResolveClientIP("10.0.0.5:443", "64.38.212.5")
	if got != "10.0.0.5" {
		t.Errorf("nil resolver: got %q, want socket peer 10.0.0.5", got)
	}
}
