package config

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/open-rails/openrails/billing"
)

func validateHyperSwitch(cfg *billing.Config) error {
	if cfg == nil || cfg.HyperSwitch == (billing.HyperSwitchConfig{}) {
		return nil
	}
	for key, raw := range map[string]string{"api_base_url": cfg.HyperSwitch.APIBaseURL, "sdk_url": cfg.HyperSwitch.SDKURL} {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
			return fmt.Errorf("hyperswitch.%s must be an absolute HTTPS URL without credentials, query or fragment", key)
		}
		if u.Scheme == "http" {
			if ip := net.ParseIP(u.Hostname()); ip == nil || !ip.IsLoopback() {
				return fmt.Errorf("hyperswitch.%s requires HTTPS outside a development sandbox literal loopback fixture", key)
			}
		}
	}
	return nil
}

// HyperSwitchFixture reports a HyperSwitch deployment over http on a loopback
// IP literal: a sandbox fixture, never beside a live PSP.
func HyperSwitchFixture(cfg *billing.Config) bool {
	if cfg == nil {
		return false
	}
	for _, raw := range []string{cfg.HyperSwitch.APIBaseURL, cfg.HyperSwitch.SDKURL} {
		if u, err := url.Parse(strings.TrimSpace(raw)); err == nil && u.Scheme == "http" {
			if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() {
				return true
			}
		}
	}
	return false
}
