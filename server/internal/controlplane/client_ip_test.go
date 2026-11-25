package controlplane

import (
	"slices"
	"testing"

	"github.com/open-rails/authkit"
	"github.com/open-rails/openrails/internal/shared/iputil"
)

func TestClientIPPostureFollowsTheEngine(t *testing.T) {
	proxies := []string{"10.0.0.0/8"}
	for name, c := range map[string]struct {
		h       authkit.HTTPConfig
		trusted []string
	}{
		"no proxies":                 {authkit.HTTPConfig{}, nil},
		"the direct peer":            {authkit.HTTPConfig{DirectPeerIP: true}, nil},
		"the engine's proxies":       {authkit.HTTPConfig{}, proxies},
		"the same proxies, split":    {authkit.HTTPConfig{TrustedProxies: append(slices.Clone(iputil.CloudflareRanges), proxies...)}, proxies},
		"the same proxies, restated": {authkit.HTTPConfig{TrustedProxies: proxies, CloudflareProxies: iputil.CloudflareRanges}, proxies},
	} {
		if err := clientIPPosture(&c.h, c.trusted); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.h.DirectPeerIP || !slices.Equal(c.h.TrustedProxies, c.trusted) || !slices.Equal(c.h.CloudflareProxies, iputil.CloudflareRanges) {
			t.Errorf("%s: AuthKit takes trusted_proxies and Cloudflare's ranges: %+v", name, c.h)
		}
	}
	for name, c := range map[string]struct {
		h       authkit.HTTPConfig
		trusted []string
	}{
		"direct peer beside trusted_proxies": {authkit.HTTPConfig{DirectPeerIP: true}, proxies},
		"direct peer beside its own proxies": {authkit.HTTPConfig{DirectPeerIP: true, CloudflareProxies: iputil.CloudflareRanges}, nil},
		"proxies of its own":                 {authkit.HTTPConfig{TrustedProxies: []string{"192.168.0.0/16"}}, proxies},
		"a Cloudflare subset":                {authkit.HTTPConfig{TrustedProxies: proxies, CloudflareProxies: []string{"173.245.48.0/20"}}, proxies},
	} {
		if err := clientIPPosture(&c.h, c.trusted); err == nil {
			t.Errorf("%s: want refused", name)
		}
	}
}
