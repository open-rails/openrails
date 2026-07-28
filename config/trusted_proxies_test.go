package config

import "testing"

func TestValidateSourceCIDRs(t *testing.T) {
	cases := []struct {
		name    string
		cidrs   []string
		wantErr bool
	}{
		{"nil is fine (trust nothing)", nil, false},
		{"empty slice is fine", []string{}, false},
		{"single valid CIDR", []string{"10.0.0.0/8"}, false},
		{"multiple valid CIDRs", []string{"10.0.0.0/8", "192.168.0.0/16"}, false},
		{"malformed CIDR rejected", []string{"not-a-cidr"}, true},
		{"bare IP without prefix rejected", []string{"10.0.0.5"}, true},
		{"one good one bad rejected", []string{"10.0.0.0/8", "garbage"}, true},
		// SEC-19: /0 trusts every source. For trusted_proxies that makes every
		// X-Forwarded-For authoritative; for the CCBill allowlist it accepts a
		// forged callback from anywhere (that rail has no HMAC).
		{"ipv4 default route rejected", []string{"0.0.0.0/0"}, true},
		{"ipv6 default route rejected", []string{"::/0"}, true},
		{"one good one wide-open rejected", []string{"10.0.0.0/8", "0.0.0.0/0"}, true},
		{"a /1 is still the operator's call", []string{"0.0.0.0/1"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSourceCIDRs(tc.cidrs)
			if tc.wantErr != (err != nil) {
				t.Fatalf("validateSourceCIDRs(%v) err=%v, wantErr=%v", tc.cidrs, err, tc.wantErr)
			}
		})
	}
}

// TestValidateRejectsMalformedTrustedProxy pins that the top-level Validate
// entry point actually calls the trusted_proxies check (#746) — a typo'd CIDR
// must fail boot, never silently no-op into an unintended trust posture.
func TestValidateRejectsMalformedTrustedProxy(t *testing.T) {
	cfg := &Config{
		Env:               "development",
		ProviderWriteMode: ProviderWriteModeFull,
		TrustedProxies:    []string{"definitely-not-a-cidr"},
	}
	if err := Validate(cfg); err == nil {
		t.Fatal("Validate() should reject a malformed trusted_proxies entry")
	}
}

// SEC-19: the CCBill source allowlist is authentication, so Validate must reject
// both a typo and a wide-open entry.
func TestValidateRejectsBadCCBillWebhookIPAllowlist(t *testing.T) {
	for _, entry := range []string{"definitely-not-a-cidr", "0.0.0.0/0"} {
		cfg := &Config{
			Env:                      "development",
			ProviderWriteMode:        ProviderWriteModeFull,
			CCBillWebhookIPAllowlist: []string{entry},
		}
		if err := Validate(cfg); err == nil {
			t.Fatalf("Validate() should reject ccbill_webhook_ip_allowlist entry %q", entry)
		}
	}
	if err := validateSourceCIDRs([]string{"127.0.0.1/32"}); err != nil {
		t.Fatalf("a loopback allowlist entry must be accepted: %v", err)
	}
}
