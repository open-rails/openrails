package config

import "testing"

func TestValidateTrustedProxies(t *testing.T) {
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTrustedProxies(tc.cidrs)
			if tc.wantErr != (err != nil) {
				t.Fatalf("validateTrustedProxies(%v) err=%v, wantErr=%v", tc.cidrs, err, tc.wantErr)
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
