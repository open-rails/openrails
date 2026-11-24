package controlplane

import (
	"testing"

	"github.com/open-rails/authkit"
)

func TestClientIPPostureFollowsTheEngine(t *testing.T) {
	proxies := []string{"10.0.0.0/8"}
	var h authkit.HTTPConfig
	if err := clientIPPosture(&h, nil); err != nil || !h.DirectPeerIP || len(h.TrustedProxies) != 0 {
		t.Fatalf("no proxies is the direct peer: %+v, %v", h, err)
	}
	h = authkit.HTTPConfig{}
	if err := clientIPPosture(&h, proxies); err != nil || h.DirectPeerIP || len(h.TrustedProxies) != 1 {
		t.Fatalf("the engine's proxies are AuthKit's: %+v, %v", h, err)
	}
	h = authkit.HTTPConfig{TrustedProxies: []string{"10.0.0.0/8"}, CloudflareProxies: []string{"173.245.48.0/20"}}
	if err := clientIPPosture(&h, []string{"173.245.48.0/20", "10.0.0.0/8"}); err != nil {
		t.Fatalf("a Cloudflare split of the same proxies is one posture: %v", err)
	}
	for name, c := range map[string]struct {
		h       authkit.HTTPConfig
		trusted []string
	}{
		"direct peer beside trusted_proxies": {authkit.HTTPConfig{DirectPeerIP: true}, proxies},
		"direct peer beside its own proxies": {authkit.HTTPConfig{DirectPeerIP: true, CloudflareProxies: proxies}, proxies},
		"proxies of its own":                 {authkit.HTTPConfig{TrustedProxies: []string{"192.168.0.0/16"}}, proxies},
	} {
		if err := clientIPPosture(&c.h, c.trusted); err == nil {
			t.Errorf("%s: want refused", name)
		}
	}
}
