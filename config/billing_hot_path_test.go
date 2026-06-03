package config

import "testing"

// TestBillingHotPath_EffectiveFailPolicy proves the #248 default is fail_closed
// (the safe default), never an accidental/silent value.
func TestBillingHotPath_EffectiveFailPolicy(t *testing.T) {
	cases := []struct {
		name string
		cfg  *BillingHotPathConfig
		want string
	}{
		{"nil defaults closed", nil, BillingFailClosed},
		{"empty defaults closed", &BillingHotPathConfig{}, BillingFailClosed},
		{"explicit closed", &BillingHotPathConfig{FailPolicy: "fail_closed"}, BillingFailClosed},
		{"explicit open", &BillingHotPathConfig{FailPolicy: "fail_open"}, BillingFailOpen},
		{"case/space normalized", &BillingHotPathConfig{FailPolicy: "  Fail_Open "}, BillingFailOpen},
	}
	for _, c := range cases {
		if got := c.cfg.EffectiveFailPolicy(); got != c.want {
			t.Errorf("%s: EffectiveFailPolicy() = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestValidateBillingHotPath_RejectsUnknown proves an unknown policy fails at
// config load: the policy must be explicit and known (#248).
func TestValidateBillingHotPath_RejectsUnknown(t *testing.T) {
	if err := validateBillingHotPath(nil); err != nil {
		t.Errorf("nil config must validate, got %v", err)
	}
	if err := validateBillingHotPath(&BillingHotPathConfig{}); err != nil {
		t.Errorf("empty (default) config must validate, got %v", err)
	}
	for _, ok := range []string{"fail_closed", "fail_open", "FAIL_OPEN", "  fail_closed "} {
		if err := validateBillingHotPath(&BillingHotPathConfig{FailPolicy: ok}); err != nil {
			t.Errorf("policy %q must validate, got %v", ok, err)
		}
	}
	for _, bad := range []string{"open", "closed", "fail-open", "silent", "maybe"} {
		if err := validateBillingHotPath(&BillingHotPathConfig{FailPolicy: bad}); err == nil {
			t.Errorf("policy %q must be rejected", bad)
		}
	}
}
